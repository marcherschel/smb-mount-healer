package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/health"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/inventory"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// minSoftGap rate-limits rounds triggered by Pod/PV changes (pod status
// updates are frequent). ProbeNow requests are never delayed.
const minSoftGap = time.Second

// healthValues are the values of the mount_status enum gauge.
var healthValues = []string{
	string(v1alpha1.HealthHealthy), string(v1alpha1.HealthSuspect), string(v1alpha1.HealthBroken),
	string(v1alpha1.HealthErrored), string(v1alpha1.HealthUnknown),
}

// severity orders health states for aggregation (worst wins).
func severity(h v1alpha1.Health) int {
	switch h {
	case v1alpha1.HealthBroken:
		return 4
	case v1alpha1.HealthSuspect:
		return 3
	case v1alpha1.HealthErrored:
		return 2
	case v1alpha1.HealthUnknown:
		return 1
	case v1alpha1.HealthHealthy:
		return 0
	}
	return 1
}

// targetState is the prober's per-path state.
type targetState struct {
	target   inventory.ProbeTarget
	rec      health.Record
	result   hostfs.ProbeResult
	probedAt time.Time // zero: never probed by this process
	nextDue  time.Time
}

// mountTransition is one health change, for Events.
type mountTransition struct {
	smbMount string
	kind     v1alpha1.MountKind
	path     string
	pod      string // "namespace/name" for pod mounts of live pods
	from, to v1alpha1.Health
	reason   v1alpha1.FailureReason
}

// nodeSnapshot is what one prober round hands to the publisher.
type nodeSnapshot struct {
	seq         uint64
	time        time.Time
	view        inventory.NodeView
	health      map[string]health.Record // by probe-target path (every tracked target)
	transitions []mountTransition
}

// ProberRunnable builds the node inventory, probes due mounts with bounded
// concurrency, advances the health FSM and hands a snapshot to the
// SmbMountPublisher. Cadence: CHECK_INTERVAL for settled mounts,
// RECHECK_DELAY while a failure streak is being confirmed (or a slow one
// waits out SLOW_RECOVERY_WINDOW, so a flapping mount is caught failing
// again), immediately for
// new targets and ProbeNow requests. Debounce state is rehydrated from the
// node's SmbMounts before the first round.
type ProberRunnable struct {
	w    *Worker
	hcfg health.Config

	hard signal // ProbeNow requests
	soft signal // Pod/PV changes

	roundMu sync.Mutex // serialises rounds; guards the fields below it
	states  map[string]*targetState
	// restored holds records rehydrated from SmbMount status until the
	// first successful round consumed them.
	restored   map[string]health.Record
	rehydrated bool
	series     map[string]prometheus.Labels

	mu         sync.Mutex // guards requests and seq
	requests   map[string]bool
	requestAll bool
	startedSeq uint64
	lastRound  time.Time

	tick atomic.Int64 // unix nanos of the last loop iteration (healthz)
}

func newProber(w *Worker) *ProberRunnable {
	p := &ProberRunnable{
		w: w,
		hcfg: health.Config{
			RecheckAttempts:    w.cfg.RecheckAttempts,
			SlowConfirmWindow:  w.cfg.SlowConfirmWindow,
			SlowRecoveryWindow: w.cfg.SlowRecoveryWindow,
		},
		hard:     newSignal(),
		soft:     newSignal(),
		states:   map[string]*targetState{},
		series:   map[string]prometheus.Labels{},
		requests: map[string]bool{},
	}
	p.setTick()
	return p
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every worker runs.
func (*ProberRunnable) NeedLeaderElection() bool { return false }

// RequestProbe makes path ("" = every mount) due in the next round and wakes
// the loop. It never blocks (HostExecutor.ProbeNow).
func (p *ProberRunnable) RequestProbe(path string) {
	p.mu.Lock()
	if path == "" {
		p.requestAll = true
	} else {
		p.requests[path] = true
	}
	p.mu.Unlock()
	p.hard.notify()
}

// Refresh wakes the loop for a round (inventory rebuilt and published,
// only due mounts probed) without forcing any probe. It never blocks.
func (p *ProberRunnable) Refresh() { p.hard.notify() }

// nextRoundSeq is the sequence number of the first round that is
// guaranteed to serve a request made now.
func (p *ProberRunnable) nextRoundSeq() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startedSeq + 1
}

func (p *ProberRunnable) setTick()            { p.tick.Store(p.w.clock.Now().UnixNano()) }
func (p *ProberRunnable) lastTick() time.Time { return time.Unix(0, p.tick.Load()) }

// staleAfter is how long the loop may go without ticking before healthz
// fails: a full round can take several STAT_TIMEOUTs, and the loop sleeps up
// to CHECK_INTERVAL between rounds.
func (p *ProberRunnable) staleAfter() time.Duration {
	return 3*p.w.cfg.CheckInterval + p.w.cfg.StartupGrace + 30*time.Second
}

// Start implements manager.Runnable.
func (p *ProberRunnable) Start(ctx context.Context) error {
	log := p.w.log.WithName("prober")
	ctx = logr.NewContext(ctx, log)
	p.setTick()
	if !p.w.cache.WaitForCacheSync(ctx) {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("prober: cache did not sync")
	}
	for _, obj := range []client.Object{&corev1.Pod{}, &corev1.PersistentVolume{}} {
		if err := p.w.onChange(ctx, obj, p.soft.notify); err != nil {
			return err
		}
	}
	if grace := p.w.cfg.StartupGrace; grace > 0 {
		log.Info("waiting startup grace before the first probe round", "grace", grace)
		select {
		case <-ctx.Done():
			return nil
		case <-p.w.clock.After(grace):
		}
	}
	for {
		p.setTick()
		next, err := p.Round(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error(err, "probe round failed")
		}
		if !p.wait(ctx, next) {
			return nil
		}
	}
}

// wait sleeps until next, a ProbeNow request, or (rate-limited) a Pod/PV
// change. It returns false when ctx is done.
func (p *ProberRunnable) wait(ctx context.Context, next time.Time) bool {
	for {
		d := next.Sub(p.w.clock.Now())
		if d <= 0 {
			return true
		}
		timer := p.w.clock.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C():
			return true
		case <-p.hard:
			timer.Stop()
			return true
		case <-p.soft:
			timer.Stop()
			p.mu.Lock()
			earliest := p.lastRound.Add(minSoftGap)
			p.mu.Unlock()
			if earliest.Before(next) {
				next = earliest
			}
		}
	}
}

// Round runs one prober round: gather the node inventory, probe the due
// targets, step the health FSM, update the metrics and hand a snapshot to
// the publisher. It returns when the next round is due. A failed inventory
// read publishes nothing (so the publisher never cleans up on partial data)
// and keeps pending ProbeNow requests for the next round.
func (p *ProberRunnable) Round(ctx context.Context) (time.Time, error) {
	p.roundMu.Lock()
	defer p.roundMu.Unlock()
	log := p.w.logger(ctx)

	p.mu.Lock()
	p.startedSeq++
	seq := p.startedSeq
	reqs, all := p.requests, p.requestAll
	p.requests, p.requestAll = map[string]bool{}, false
	p.mu.Unlock()

	now := p.w.clock.Now()
	retry := now.Add(min(p.w.cfg.RecheckDelay, p.w.cfg.CheckInterval))
	if err := p.rehydrate(ctx); err != nil {
		p.requeue(reqs, all)
		return retry, err
	}
	in, err := p.gather(ctx, now)
	if err != nil {
		p.requeue(reqs, all)
		return retry, err
	}
	view := inventory.Build(in)
	targets := view.ProbeTargets()

	live := make(map[string]bool, len(targets))
	var due []*targetState
	for _, t := range targets {
		live[t.Path] = true
		st := p.states[t.Path]
		if st == nil {
			st = &targetState{rec: health.Record{State: v1alpha1.HealthUnknown}}
			if r, ok := p.restored[t.Path]; ok {
				st.rec = r
			}
			p.states[t.Path] = st
		}
		st.target = t
		if all || reqs[t.Path] || st.probedAt.IsZero() || !now.Before(st.nextDue) {
			due = append(due, st)
		}
	}
	for path := range p.states {
		if !live[path] {
			delete(p.states, path)
		}
	}
	p.restored = nil

	p.probe(ctx, in.Snapshot, due)
	idx := indexView(view)
	var transitions []mountTransition
	for _, st := range due {
		if tr := p.step(log, st, idx); tr != nil {
			transitions = append(transitions, *tr)
		}
	}
	p.updateSeries(idx)
	p.w.m.ProbesHung.Set(float64(p.w.host.Prober.Hung()))
	p.w.m.UnknownGlobalmounts.Set(float64(len(view.UnknownGlobalmounts)))

	snap := &nodeSnapshot{seq: seq, time: now, view: view, health: make(map[string]health.Record, len(p.states)),
		transitions: transitions}
	next := now.Add(p.w.cfg.CheckInterval)
	for path, st := range p.states {
		snap.health[path] = st.rec
		if st.nextDue.Before(next) {
			next = st.nextDue
		}
	}
	for _, pod := range in.Pods {
		if end := podStart(&pod).Add(p.w.cfg.PodStartupGrace); end.After(now) && end.Before(next) {
			next = end
		}
	}
	p.mu.Lock()
	p.lastRound = now
	p.mu.Unlock()
	p.w.Publisher.offer(snap)
	return next, nil
}

func (p *ProberRunnable) requeue(reqs map[string]bool, all bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for path := range reqs {
		p.requests[path] = true
	}
	p.requestAll = p.requestAll || all
}

// rehydrate loads the debounce state of this node's SmbMounts once: the
// top-level health/debounce belong to the globalmount when it is mounted;
// pod mounts carry health and reason only.
func (p *ProberRunnable) rehydrate(ctx context.Context) error {
	if p.rehydrated {
		return nil
	}
	var list v1alpha1.SmbMountList
	if err := p.w.c.List(ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: p.w.node}); err != nil {
		return fmt.Errorf("list SmbMounts for rehydration: %w", err)
	}
	restored := map[string]health.Record{}
	for i := range list.Items {
		sm := &list.Items[i]
		if sm.Spec.NodeName != p.w.node {
			continue
		}
		st := &sm.Status
		if g := st.Globalmount; g != nil && g.Mounted && sm.Spec.GlobalmountPath != "" && st.Health != "" {
			restored[sm.Spec.GlobalmountPath] = health.FromStatus(st.Health, st.Reason, st.Debounce)
		}
		for _, pm := range st.PodMounts {
			if pm.Health != "" && pm.UID != "" {
				restored[p.w.layout.PodVolumeMountPath(pm.UID, sm.Spec.PVName)] = health.FromStatus(pm.Health, pm.Reason, nil)
			}
		}
	}
	p.restored, p.rehydrated = restored, true
	p.w.logger(ctx).Info("rehydrated health state from SmbMounts", "smbMounts", len(list.Items), "records", len(restored))
	return nil
}

// gather reads everything inventory.Build needs. Directory listing errors
// fail the round; an Inspect error only makes that directory count as
// existing and empty (never claim shadow data that was not seen).
func (p *ProberRunnable) gather(ctx context.Context, now time.Time) (inventory.Input, error) {
	var pvs corev1.PersistentVolumeList
	if err := p.w.c.List(ctx, &pvs); err != nil {
		return inventory.Input{}, fmt.Errorf("list PVs: %w", err)
	}
	var pods corev1.PodList
	if err := p.w.c.List(ctx, &pods); err != nil {
		return inventory.Input{}, fmt.Errorf("list pods: %w", err)
	}
	h := p.w.host
	snap, err := h.Mounts.Snapshot(ctx)
	if err != nil {
		return inventory.Input{}, fmt.Errorf("read mountinfo: %w", err)
	}
	podDirs, err := h.Dirs.ListPodVolumeDirs(ctx)
	if err != nil {
		return inventory.Input{}, fmt.Errorf("list pod volume dirs: %w", err)
	}
	gmDirs, err := h.Dirs.ListGlobalmountDirs(ctx)
	if err != nil {
		return inventory.Input{}, fmt.Errorf("list globalmount dirs: %w", err)
	}
	facts := map[string]hostfs.DirFacts{}
	for _, path := range inventory.DirsToInspect(snap, podDirs, gmDirs) {
		f, err := h.Dirs.Inspect(ctx, path)
		if err != nil {
			p.w.logger(ctx).Error(err, "inspect directory failed; treating it as empty this round", "path", path)
			f = hostfs.DirFacts{Exists: true, IsDir: true, Empty: true}
		}
		facts[path] = f
	}
	return inventory.Input{
		NodeName:        p.w.node,
		Layout:          p.w.layout,
		PVs:             pvs.Items,
		Pods:            pods.Items,
		Snapshot:        snap,
		PodVolumeDirs:   podDirs,
		GlobalmountDirs: gmDirs,
		DirFacts:        facts,
		Now:             now,
		PodStartupGrace: p.w.cfg.PodStartupGrace,
	}, nil
}

func podStart(pod *corev1.Pod) time.Time {
	if pod.Status.StartTime != nil {
		return pod.Status.StartTime.Time
	}
	return pod.CreationTimestamp.Time
}

// probe runs the due probes with at most PROBE_CONCURRENCY in flight.
func (p *ProberRunnable) probe(ctx context.Context, snap hostfs.MountSnapshot, due []*targetState) {
	sem := make(chan struct{}, max(p.w.cfg.ProbeConcurrency, 1))
	var wg sync.WaitGroup
	for _, st := range due {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			st.result = p.w.host.Prober.Probe(ctx, st.target.Path, snap)
			st.probedAt = p.w.clock.Now()
		}()
	}
	wg.Wait()
}

// step feeds one probe result into the FSM and the metrics.
func (p *ProberRunnable) step(log logr.Logger, st *targetState, idx viewIndex) *mountTransition {
	m, r, t := p.w.m, st.result, st.target
	kind := string(t.Kind)
	m.MountChecks.WithLabelValues(kind, string(r.Class)).Inc()
	m.MountCheckDuration.WithLabelValues(kind).Observe(r.Duration.Seconds())

	next, tr := health.Step(st.rec, r.Class, st.probedAt, p.hcfg)
	st.rec = next
	interval := p.w.cfg.CheckInterval
	if next.State == v1alpha1.HealthSuspect || (next.State == v1alpha1.HealthErrored && next.Failures > 0) {
		interval = p.w.cfg.RecheckDelay
	}
	st.nextDue = st.probedAt.Add(interval)
	if r.Class != hostfs.ProbeOK {
		log.V(1).Info("probe failed", "path", t.Path, "pv", t.PVName, "kind", kind, "class", r.Class,
			"detail", r.Detail, "health", next.State, "failures", next.Failures)
	}
	if tr == nil {
		return nil
	}
	m.HealthTransitions.WithLabelValues(kind, string(tr.From), string(tr.To)).Inc()
	if tr.FalseAlarm {
		m.FalseAlarms.WithLabelValues(kind, string(tr.Reason)).Inc()
	}
	out := &mountTransition{smbMount: t.SmbMountName, kind: t.Kind, path: t.Path, from: tr.From, to: tr.To, reason: tr.Reason}
	if pm, ok := idx.pod(t.PVName, t.PodUID); ok && t.Kind == v1alpha1.MountKindPod {
		out.pod = pm.Namespace + "/" + pm.Pod
	}
	log.Info("mount health changed", "path", t.Path, "pv", t.PVName, "kind", kind, "pod", out.pod,
		"from", tr.From, "to", tr.To, "reason", tr.Reason, "falseAlarm", tr.FalseAlarm, "detail", r.Detail)
	return out
}

// updateSeries sets the mount_status enum gauge for every tracked target
// (aggregated, worst first, when several pods of one workload share a label
// set) and deletes the series of vanished mounts.
func (p *ProberRunnable) updateSeries(idx viewIndex) {
	type agg struct {
		labels prometheus.Labels
		status v1alpha1.Health
	}
	desired := map[string]*agg{}
	for _, st := range p.states {
		t := st.target
		var ns, wl string
		if pm, ok := idx.pod(t.PVName, t.PodUID); ok && t.Kind == v1alpha1.MountKindPod {
			ns, wl = pm.Namespace, pm.Workload
		}
		share := idx.share[t.PVName]
		key := string(t.Kind) + "\x00" + t.PVName + "\x00" + share + "\x00" + ns + "\x00" + wl
		status := st.rec.State
		if status == "" {
			status = v1alpha1.HealthUnknown
		}
		if a, ok := desired[key]; ok {
			if severity(status) > severity(a.status) {
				a.status = status
			}
			continue
		}
		desired[key] = &agg{status: status, labels: prometheus.Labels{
			metrics.LabelKind: string(t.Kind), metrics.LabelPV: t.PVName, metrics.LabelShare: share,
			metrics.LabelWorkloadNamespace: ns, metrics.LabelWorkload: wl,
		}}
	}
	for key, base := range p.series {
		if desired[key] == nil {
			metrics.DeleteEnum(p.w.m.MountStatus, base)
			delete(p.series, key)
		}
	}
	for key, a := range desired {
		metrics.SetEnum(p.w.m.MountStatus, a.labels, metrics.LabelStatus, healthValues, string(a.status))
		p.series[key] = a.labels
	}
}

// viewIndex looks up volumes and pod mounts of a NodeView.
type viewIndex struct {
	share map[string]string
	pods  map[string]map[types.UID]inventory.PodMountView
}

func indexView(v inventory.NodeView) viewIndex {
	idx := viewIndex{share: map[string]string{}, pods: map[string]map[types.UID]inventory.PodMountView{}}
	for _, vol := range v.Volumes {
		idx.share[vol.PVName] = vol.Share
		m := make(map[types.UID]inventory.PodMountView, len(vol.PodMounts))
		for _, pm := range vol.PodMounts {
			m[pm.UID] = pm
		}
		idx.pods[vol.PVName] = m
	}
	return idx
}

func (x viewIndex) pod(pv string, uid types.UID) (inventory.PodMountView, bool) {
	pm, ok := x.pods[pv][uid]
	return pm, ok
}

// onChange calls fn on every add, update and delete of obj's kind in the cache.
func (w *Worker) onChange(ctx context.Context, obj client.Object, fn func()) error {
	inf, err := w.cache.GetInformer(ctx, obj)
	if err != nil {
		return fmt.Errorf("informer for %T: %w", obj, err)
	}
	_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { fn() },
		UpdateFunc: func(any, any) { fn() },
		DeleteFunc: func(any) { fn() },
	})
	return err
}
