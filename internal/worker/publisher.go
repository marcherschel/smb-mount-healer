package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	ac "github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/inventory"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// Event reasons emitted by the worker.
const (
	EventMountBroken          = "MountBroken"
	EventMountRecovered       = "MountRecovered"
	EventShadowDataQuarantine = "ShadowDataQuarantined"
)

// appliedState is what the publisher last wrote for one SmbMount.
type appliedState struct {
	fp     string
	status v1alpha1.SmbMountStatus
}

// SmbMountPublisher creates the node's SmbMount objects (immutable spec,
// Node ownerReference, node/pv labels) and server-side-applies the
// worker-owned status fields under FieldManagerWorker. A status is written
// when its content changed, otherwise at most once per STATUS_HEARTBEAT (to
// refresh lastProbeTime). SmbMounts whose PV has no globalmount, pod
// directory or pod reference left on the node are deleted, except while a
// remediation is active on them: then the worker publishes the "gone" state
// so the control plane sees the globalmount directory disappear. An idle
// volume (see idleVolume) gets no SmbMount either, unless a remediation is
// active on it or quarantined shadow data of it is pending on the node.
type SmbMountPublisher struct {
	w    *Worker
	kick signal

	mu          sync.Mutex
	latest      *nodeSnapshot
	transitions []mountTransition
	pubSeq      uint64
	pubCh       chan struct{} // closed and replaced after every Publish
	published   atomic.Bool

	syncMu  sync.Mutex // serialises Publish; guards applied
	applied map[string]appliedState
}

func newPublisher(w *Worker) *SmbMountPublisher {
	return &SmbMountPublisher{w: w, kick: newSignal(), pubCh: make(chan struct{}), applied: map[string]appliedState{}}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*SmbMountPublisher) NeedLeaderElection() bool { return false }

// offer hands the result of a prober round to the publisher (latest wins;
// transitions accumulate until published).
func (p *SmbMountPublisher) offer(s *nodeSnapshot) {
	p.mu.Lock()
	p.latest = s
	p.transitions = append(p.transitions, s.transitions...)
	p.mu.Unlock()
	p.kick.notify()
}

func (p *SmbMountPublisher) publishedOnce() bool { return p.published.Load() }

// waitPublished blocks until a snapshot with sequence >= seq was published,
// timeout passed on the worker clock, or ctx is done. It reports whether seq
// was published.
func (p *SmbMountPublisher) waitPublished(ctx context.Context, seq uint64, timeout time.Duration) bool {
	deadline := p.w.clock.After(timeout)
	for {
		p.mu.Lock()
		done, ch := p.pubSeq >= seq, p.pubCh
		p.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// Start implements manager.Runnable: publish on every prober round and on
// SmbMount/SmbQuarantine changes (and retry at least every STATUS_HEARTBEAT).
func (p *SmbMountPublisher) Start(ctx context.Context) error {
	log := p.w.log.WithName("publisher")
	ctx = logr.NewContext(ctx, log)
	if !p.w.cache.WaitForCacheSync(ctx) {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("publisher: cache did not sync")
	}
	for _, obj := range []client.Object{&v1alpha1.SmbQuarantine{}, &v1alpha1.SmbMount{}} {
		if err := p.w.onChange(ctx, obj, p.kick.notify); err != nil {
			return err
		}
	}
	for {
		timer := p.w.clock.NewTimer(p.w.cfg.StatusHeartbeat)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-p.kick:
		case <-timer.C():
		}
		timer.Stop()
		if err := p.Publish(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "publishing SmbMounts failed")
		}
	}
}

// Publish syncs the SmbMount objects of this node with the latest prober
// snapshot. It is a no-op until the first snapshot arrived.
func (p *SmbMountPublisher) Publish(ctx context.Context) error {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	p.mu.Lock()
	snap, transitions := p.latest, p.transitions
	p.transitions = nil
	p.mu.Unlock()
	if snap == nil {
		return nil
	}
	err := p.sync(ctx, snap, transitions)
	p.mu.Lock()
	if snap.seq > p.pubSeq {
		p.pubSeq = snap.seq
	}
	close(p.pubCh)
	p.pubCh = make(chan struct{})
	p.mu.Unlock()
	p.published.Store(true)
	return err
}

func (p *SmbMountPublisher) sync(ctx context.Context, snap *nodeSnapshot, transitions []mountTransition) error {
	log := p.w.logger(ctx)
	w := p.w
	owner, err := w.nodeOwner(ctx)
	if err != nil {
		return err
	}
	var list v1alpha1.SmbMountList
	if err := w.c.List(ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: w.node}); err != nil {
		return fmt.Errorf("list SmbMounts: %w", err)
	}
	live := map[string]*v1alpha1.SmbMount{}
	for i := range list.Items {
		if sm := &list.Items[i]; sm.Spec.NodeName == w.node {
			live[sm.Name] = sm
		}
	}
	pending, err := p.pendingQuarantines(ctx)
	if err != nil {
		return err
	}

	var errs []error
	objects := map[string]*v1alpha1.SmbMount{}
	seen := map[string]bool{}
	for _, vol := range snap.view.Volumes {
		name := vol.SmbMountName
		sm := live[name]
		if idleVolume(vol) && pending[vol.PVName] == 0 && !remediating(sm) {
			continue // not seen: an existing SmbMount is deleted below
		}
		seen[name] = true
		spec := smbMountSpec(w.node, vol)
		switch {
		case sm != nil && !sm.DeletionTimestamp.IsZero():
			continue // being deleted; recreated on a later round
		case sm != nil && sm.Spec != spec:
			// The PV was recreated under the same name: the spec is
			// immutable, so replace the object.
			log.Info("SmbMount spec changed; recreating it", "smbMount", name)
			if err := p.delete(ctx, sm); err != nil {
				errs = append(errs, err)
			}
			continue
		case sm == nil:
			created, err := p.create(ctx, name, spec, owner)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			sm = created
		}
		objects[name] = sm
		if err := p.apply(ctx, sm, vol, snap, pending[vol.PVName]); err != nil {
			errs = append(errs, err)
		}
	}
	for name, sm := range live {
		if seen[name] || !sm.DeletionTimestamp.IsZero() {
			continue
		}
		if remediating(sm) {
			vol := inventory.VolumeView{PVName: sm.Spec.PVName, Share: sm.Spec.Share, VolumeHandle: sm.Spec.VolumeHandle,
				SmbMountName: name, Globalmount: inventory.GlobalmountView{Path: sm.Spec.GlobalmountPath}}
			if err := p.apply(ctx, sm, vol, snap, pending[sm.Spec.PVName]); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		log.Info("deleting SmbMount: no mount, non-empty directory or pod reference left on this node", "smbMount", name)
		if err := p.delete(ctx, sm); err != nil {
			errs = append(errs, err)
		}
	}
	p.emit(transitions, objects)
	return errors.Join(errs...)
}

// idleVolume reports whether vol is only an empty, unmounted globalmount
// directory with no pod referencing the PV and no orphan pod dir: the
// leftover of an earlier unstage, with nothing to probe or heal.
func idleVolume(vol inventory.VolumeView) bool {
	gm := vol.Globalmount
	return len(vol.PodMounts) == 0 && len(vol.OrphanDirs) == 0 && !gm.Mounted && gm.Dir.Exists && gm.Dir.Empty
}

// remediating reports whether a remediation is active on sm (nil-safe).
func remediating(sm *v1alpha1.SmbMount) bool {
	return sm != nil && sm.Status.Remediation != nil && sm.Status.Remediation.Active
}

// pendingQuarantines counts this node's SmbQuarantine objects per PV.
func (p *SmbMountPublisher) pendingQuarantines(ctx context.Context) (map[string]int32, error) {
	var list v1alpha1.SmbQuarantineList
	if err := p.w.c.List(ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: p.w.node}); err != nil {
		return nil, fmt.Errorf("list SmbQuarantines: %w", err)
	}
	out := map[string]int32{}
	for i := range list.Items {
		q := &list.Items[i]
		if q.Spec.NodeName == p.w.node && q.DeletionTimestamp.IsZero() {
			out[q.Spec.PVName]++
		}
	}
	return out, nil
}

func (p *SmbMountPublisher) create(ctx context.Context, name string, spec v1alpha1.SmbMountSpec, owner metav1.OwnerReference) (*v1alpha1.SmbMount, error) {
	sm := &v1alpha1.SmbMount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Labels:          p.w.objectLabels(spec.PVName),
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: spec,
	}
	err := p.w.c.Create(ctx, sm, client.FieldOwner(v1alpha1.FieldManagerWorker))
	switch {
	case apierrors.IsAlreadyExists(err):
		// Created by us earlier, the cache has not caught up yet.
		sm = &v1alpha1.SmbMount{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	case err != nil:
		return nil, fmt.Errorf("create SmbMount %s: %w", name, err)
	default:
		p.w.logger(ctx).Info("created SmbMount", "smbMount", name, "pv", spec.PVName)
	}
	return sm, nil
}

func (p *SmbMountPublisher) delete(ctx context.Context, sm *v1alpha1.SmbMount) error {
	err := p.w.c.Delete(ctx, sm, client.Preconditions{UID: &sm.UID})
	delete(p.applied, sm.Name)
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete SmbMount %s: %w", sm.Name, err)
	}
	return nil
}

// apply writes the worker status of sm if it changed or the heartbeat is due.
func (p *SmbMountPublisher) apply(ctx context.Context, sm *v1alpha1.SmbMount, vol inventory.VolumeView,
	snap *nodeSnapshot, pending int32,
) error {
	now := p.w.clock.Now()
	prev, ok := p.applied[sm.Name]
	if !ok && sm.UID != "" {
		// First sync of this object by this process: compare with what a
		// previous worker wrote (restart must not cause a write storm).
		ws := workerFields(sm.Status)
		prev = appliedState{fp: fingerprint(ws), status: ws}
	}
	st := workerStatus(vol, snap.health, pending, snap.time, prev.status.Conditions, now)
	fp := fingerprint(st)
	if fp == prev.fp && !heartbeatDue(snap.time, prev.status.LastProbeTime, p.w.cfg.StatusHeartbeat) {
		return nil
	}
	err := p.w.c.Status().Apply(ctx, ac.SmbMount(sm.Name).WithStatus(statusApply(st)),
		client.FieldOwner(v1alpha1.FieldManagerWorker), client.ForceOwnership)
	if err != nil {
		p.w.m.SmbMountStatusWrites.WithLabelValues(metrics.ResultError).Inc()
		if apierrors.IsNotFound(err) {
			delete(p.applied, sm.Name)
			return nil
		}
		return fmt.Errorf("apply status of SmbMount %s: %w", sm.Name, err)
	}
	p.w.m.SmbMountStatusWrites.WithLabelValues(metrics.ResultSuccess).Inc()
	p.applied[sm.Name] = appliedState{fp: fp, status: st}
	return nil
}

// emit records Events for transitions into and out of Broken.
func (p *SmbMountPublisher) emit(transitions []mountTransition, objects map[string]*v1alpha1.SmbMount) {
	for _, t := range transitions {
		sm := objects[t.smbMount]
		if sm == nil || sm.UID == "" {
			continue
		}
		what := string(t.kind) + " mount " + t.path
		if t.pod != "" {
			what += " of pod " + t.pod
		}
		switch {
		case t.to == v1alpha1.HealthBroken:
			p.w.event(sm, corev1.EventTypeWarning, EventMountBroken, fmt.Sprintf("%s is Broken (%s)", what, t.reason))
		case t.from == v1alpha1.HealthBroken && t.to == v1alpha1.HealthHealthy:
			p.w.event(sm, corev1.EventTypeNormal, EventMountRecovered, what+" recovered")
		}
	}
}

// heartbeatDue reports whether an unchanged status should still be written
// to refresh lastProbeTime: the round at t is at least one STATUS_HEARTBEAT
// newer than the lastProbeTime already stored. A stuck prober therefore
// shows as a stale lastProbeTime instead of being papered over.
func heartbeatDue(t time.Time, last *metav1.Time, heartbeat time.Duration) bool {
	return last == nil || t.Sub(last.Time) >= heartbeat
}
