package sim

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
)

// Kubelet timings.
const (
	// podTerminationGrace is how long a deleted pod's containers take to stop.
	podTerminationGrace = 4 * time.Second
	// opBackoffInitial and opBackoffMax are kubelet's exponential backoff for
	// failed volume operations (nestedpendingoperations; capped at 2m10s here).
	opBackoffInitial = 500 * time.Millisecond
	opBackoffMax     = 2*time.Minute + 10*time.Second
	// orphanSweepEvery is kubelet's housekeeping of orphaned pod dirs.
	orphanSweepEvery = 10 * time.Second

	localFSType = "xfs"
	localDevice = "/dev/vda6"
	cifsOptions = "rw,vers=3.0,cache=strict,soft,dir_mode=0777,file_mode=0777"
)

// session is the cifs session behind a mount entry.
type session struct {
	server string
	gen    int
}

// retry is the exponential backoff state of one kubelet volume operation.
type retry struct {
	next     time.Time
	backoff  time.Duration
	failures int
}

func (r *retry) due(now time.Time) bool { return !now.Before(r.next) }

func (r *retry) fail(now time.Time) time.Duration {
	if r.backoff == 0 {
		r.backoff = opBackoffInitial
	} else {
		r.backoff = min(2*r.backoff, opBackoffMax)
	}
	r.failures++
	r.next = now.Add(r.backoff)
	return r.backoff
}

func (r *retry) reset() { *r = retry{} }

// volState is kubelet's actual state of world for one PV on the node.
type volState struct {
	staged  bool
	pods    map[types.UID]bool // published pods (incl. ones whose unpublish keeps failing)
	stage   retry
	unstage retry
}

// podState is kubelet's view of one pod.
type podState struct {
	ns, name  string
	pvs       []*PV
	termSeen  time.Time
	published map[string]bool
	pubRetry  map[string]*retry
	unpubTry  map[string]*retry
	running   bool
	gone      bool
}

// Kubelet is the fake kubelet of one node with the incident semantics:
//
//   - NodeStage is a no-op when the globalmount is already a mountpoint
//     (whatever is mounted there, dead or not);
//   - kubelet only stages when its actual state says the volume is not
//     staged; a globalmount detached behind its back is still "staged", so
//     NodePublish binds the bare globalmount directory (xfs on the root
//     disk) into the pod: wrong_fstype at the pod path, and the pod's
//     writes land in the globalmount dir as shadow data;
//   - NodeUnpublish fails on a non-empty pod dir (the pod stays
//     Terminating, the volume stays referenced);
//   - NodeUnstage runs when the last pod is gone; it fails on a dead cifs
//     mount and on a non-empty globalmount dir, and is retried with
//     exponential backoff capped at 2m10s;
//   - a pod that wants the volume while an unstage is pending cancels the
//     unstage (the volume stays staged in whatever state it is in);
//   - every volume operation needs a Ready CSI driver pod; a stage resolves
//     the samba address through the driver, so a driver that predates a
//     samba IP change mounts a stale (dead) session until it is restarted.
type Kubelet struct {
	n         *Node
	vols      map[string]*volState
	pods      map[types.UID]*podState
	sessions  map[int]session // mount entry ID -> cifs session
	bareBinds map[int]string  // mount entry ID -> bound local dir (globalmount)
	local     map[string]map[string]bool
	stickies  map[string]bool
	nextSweep time.Time
}

func newKubelet(n *Node) *Kubelet {
	return &Kubelet{n: n, vols: map[string]*volState{}, pods: map[types.UID]*podState{},
		sessions: map[int]session{}, bareBinds: map[int]string{}, local: map[string]map[string]bool{},
		stickies: map[string]bool{}}
}

func (k *Kubelet) vol(pv string) *volState {
	v := k.vols[pv]
	if v == nil {
		v = &volState{pods: map[types.UID]bool{}}
		k.vols[pv] = v
	}
	return v
}

func (k *Kubelet) host() *fake.Host { return k.n.Host }

func (k *Kubelet) trace(msg string, args ...any) {
	k.n.w.trace.add(Event{Kind: "kubelet", Obj: k.n.Name, Msg: fmt.Sprintf(msg, args...)})
}

func (k *Kubelet) step() {
	w := k.n.w
	now := w.Now()
	k.syncLocal()
	driverReady := k.n.driverReady()

	pods := w.listPods()
	present := map[types.UID]bool{}
	wanted := map[string]bool{} // PVs a live, non-terminating pod needs
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != k.n.Name || p.Namespace == DriverNS {
			continue
		}
		present[p.UID] = true
		ps := k.pods[p.UID]
		if ps == nil {
			ps = &podState{ns: p.Namespace, name: p.Name, pvs: w.podPVs(p), published: map[string]bool{},
				pubRetry: map[string]*retry{}, unpubTry: map[string]*retry{}}
			k.pods[p.UID] = ps
			w.podNames[p.UID] = p.Namespace + "/" + p.Name
		}
		if p.DeletionTimestamp == nil {
			for _, pv := range ps.pvs {
				wanted[pv.Name] = true
			}
		}
	}

	uids := make([]types.UID, 0, len(k.pods))
	for uid := range k.pods {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool {
		return k.pods[uids[i]].name < k.pods[uids[j]].name || (k.pods[uids[i]].name == k.pods[uids[j]].name && uids[i] < uids[j])
	})
	for _, uid := range uids {
		ps := k.pods[uid]
		if !present[uid] {
			delete(k.pods, uid)
			continue
		}
		var pod *corev1.Pod
		for i := range pods {
			if pods[i].UID == uid {
				pod = &pods[i]
			}
		}
		if pod.DeletionTimestamp != nil {
			k.teardown(pod, ps, driverReady)
			continue
		}
		k.setup(pod, ps, driverReady)
	}

	pvNames := make([]string, 0, len(k.vols))
	for pv := range k.vols {
		pvNames = append(pvNames, pv)
	}
	sort.Strings(pvNames)
	for _, name := range pvNames {
		v := k.vols[name]
		if !v.staged || len(v.pods) > 0 || wanted[name] || !v.unstage.due(now) {
			continue
		}
		k.unstage(w.pvs[name], v, driverReady)
	}

	k.writeHeartbeats(pods)
	if !now.Before(k.nextSweep) {
		k.nextSweep = now.Add(orphanSweepEvery)
		k.sweepOrphans(present)
	}
	k.updateProbeResults()
}

// setup stages and publishes the pod's volumes, then starts the pod.
func (k *Kubelet) setup(pod *corev1.Pod, ps *podState, driverReady bool) {
	w := k.n.w
	now := w.Now()
	for _, pv := range ps.pvs {
		if ps.published[pv.Name] {
			continue
		}
		r := ps.pubRetry[pv.Name]
		if r == nil {
			r = &retry{}
			ps.pubRetry[pv.Name] = r
		}
		if !r.due(now) {
			continue
		}
		v := k.vol(pv.Name)
		if v.unstage.failures > 0 {
			k.trace("unstage of %s cancelled: pod %s/%s wants the volume (volume stays staged)", pv.Name, ps.ns, ps.name)
			v.unstage.reset()
		}
		if !driverReady {
			if r.failures == 0 {
				k.trace("publish %s for %s/%s failed: CSI driver not ready", pv.Name, ps.ns, ps.name)
			}
			r.fail(now)
			continue
		}
		if !v.staged {
			if !k.stageVolume(pv, v) {
				r.fail(now)
				continue
			}
		}
		k.publish(pv, uidOf(pod), ps)
		v.pods[pod.UID] = true
		ps.published[pv.Name] = true
		r.reset()
	}
	if ps.running {
		return
	}
	for _, pv := range ps.pvs {
		if !ps.published[pv.Name] {
			return
		}
	}
	setPodRunning(pod, now)
	if err := w.store.Status().Update(w.ctx, pod); err != nil {
		w.violation("pod %s status: %v", pod.Name, err)
		return
	}
	ps.running = true
	w.trace.add(Event{Kind: "pod", Obj: ps.ns + "/" + ps.name, Msg: "running on " + k.n.Name})
}

func uidOf(p *corev1.Pod) types.UID { return p.UID }

// stageVolume runs NodeStageVolume. It returns false on failure.
func (k *Kubelet) stageVolume(pv *PV, v *volState) bool {
	w := k.n.w
	h := k.host()
	if snap, _ := h.Mounts.Snapshot(w.ctx); snap.IsMountpoint(pv.GMPath) {
		e, _ := snap.Lookup(pv.GMPath)
		k.trace("stage %s: no-op, globalmount is already a mountpoint (%s)", pv.Name, e.FSType)
		v.staged = true
		return true
	}
	if !v.stage.due(w.Now()) {
		return false
	}
	if !w.Samba.canMount(pv.Server) {
		d := v.stage.fail(w.Now())
		k.trace("stage %s failed: mount error(112): Host is down (retry in %s)", pv.Name, d)
		return false
	}
	if _, ok := h.Dirs.Dir(pv.GMPath); !ok {
		h.Dirs.SetDir(pv.GMPath, fake.Dir{})
	}
	e := h.Mounts.Mount(pv.GMPath, "cifs", pv.Source, cifsOptions)
	gen := k.n.driverGen(pv.Server)
	k.sessions[e.ID] = session{server: pv.Server, gen: gen}
	v.staged = true
	v.stage.reset()
	if gen < w.Samba.Gen(pv.Server) {
		k.trace("stage %s: mounted cifs %s with a stale session (driver resolved an old address)", pv.Name, pv.Source)
	} else {
		k.trace("stage %s: mounted cifs %s", pv.Name, pv.Source)
	}
	return true
}

// publish bind-mounts the globalmount into the pod: a bind of whatever is
// mounted there, or of the bare directory when nothing is.
func (k *Kubelet) publish(pv *PV, uid types.UID, ps *podState) {
	w := k.n.w
	h := k.host()
	path := w.layout.PodVolumeMountPath(uid, pv.Name)
	if _, ok := h.Dirs.Dir(path); !ok {
		h.Dirs.SetDir(path, fake.Dir{})
	}
	snap, _ := h.Mounts.Snapshot(w.ctx)
	if snap.IsMountpoint(path) {
		return
	}
	if top, ok := snap.Lookup(pv.GMPath); ok {
		e := h.Mounts.Mount(path, top.FSType, top.Source, top.SuperOptions)
		if s, ok := k.sessions[top.ID]; ok {
			k.sessions[e.ID] = s
		}
		if d, ok := k.bareBinds[top.ID]; ok {
			k.bareBinds[e.ID] = d
		}
		k.trace("publish %s into %s/%s: bind of the globalmount (%s)", pv.Name, ps.ns, ps.name, top.FSType)
		return
	}
	if _, ok := h.Dirs.Dir(pv.GMPath); !ok {
		h.Dirs.SetDir(pv.GMPath, fake.Dir{})
	}
	e := h.Mounts.Mount(path, localFSType, localDevice, "rw,attr2,inode64")
	k.bareBinds[e.ID] = pv.GMPath
	k.trace("publish %s into %s/%s: globalmount not mounted, bound the bare directory (%s on %s)",
		pv.Name, ps.ns, ps.name, localFSType, localDevice)
}

// teardown stops a deleted pod after its grace period and unpublishes its
// volumes; the pod object goes away once every unpublish succeeded.
func (k *Kubelet) teardown(pod *corev1.Pod, ps *podState, driverReady bool) {
	w := k.n.w
	now := w.Now()
	if ps.termSeen.IsZero() {
		ps.termSeen = now
		ps.running = false
	}
	if now.Sub(ps.termSeen) < podTerminationGrace {
		return
	}
	pvs := make([]string, 0, len(ps.published))
	for pv := range ps.published {
		pvs = append(pvs, pv)
	}
	sort.Strings(pvs)
	for _, name := range pvs {
		r := ps.unpubTry[name]
		if r == nil {
			r = &retry{}
			ps.unpubTry[name] = r
		}
		if !r.due(now) {
			continue
		}
		if !driverReady {
			r.fail(now)
			continue
		}
		path := w.layout.PodVolumeMountPath(pod.UID, name)
		h := k.host()
		if top, ok := k.topMount(path); ok {
			if s, ok := k.sessions[top.ID]; ok {
				if c, _ := w.Samba.classify(s.server, s.gen); c == hostfs.ProbeStatTimeout || c == hostfs.ProbeHung {
					bo := r.fail(now)
					if r.failures == 1 {
						k.trace("unpublish %s of %s/%s failed: unmount %s: timed out (server not responding, retry in %s)",
							name, ps.ns, ps.name, w.trace.abbrev(path), bo)
					}
					continue
				}
			}
		}
		for h.Mounts.Unmount(path) {
		}
		if d, ok := h.Dirs.Dir(path); ok && d.Entries > 0 {
			bo := r.fail(now)
			if r.failures == 1 || bo == opBackoffMax {
				k.trace("unpublish %s of %s/%s failed: remove %s: directory not empty (retry in %s)",
					name, ps.ns, ps.name, w.trace.abbrev(path), bo)
			}
			continue
		}
		h.Dirs.RemoveDir(path)
		delete(ps.published, name)
		delete(k.vol(name).pods, pod.UID)
	}
	if len(ps.published) > 0 || ps.gone {
		return
	}
	ps.gone = true
	controllerutil.RemoveFinalizer(pod, podFinalizer)
	if err := w.store.Update(w.ctx, pod); err != nil {
		w.violation("remove pod finalizer %s: %v", pod.Name, err)
		return
	}
	w.trace.add(Event{Kind: "pod", Obj: ps.ns + "/" + ps.name, Msg: "gone"})
}

// unstage runs NodeUnstageVolume for an unreferenced staged volume.
func (k *Kubelet) unstage(pv *PV, v *volState, driverReady bool) {
	w := k.n.w
	now := w.Now()
	h := k.host()
	fail := func(reason string) {
		d := v.unstage.fail(now)
		if v.unstage.failures <= 2 || d == opBackoffMax && v.unstage.failures%4 == 0 {
			k.trace("unstage %s failed: %s (attempt %d, retry in %s)", pv.Name, reason, v.unstage.failures, d)
		}
	}
	if !driverReady {
		fail("CSI driver not ready")
		return
	}
	snap, _ := h.Mounts.Snapshot(w.ctx)
	if top, ok := snap.Lookup(pv.GMPath); ok {
		if s, ok := k.sessions[top.ID]; ok {
			if c, _ := w.Samba.classify(s.server, s.gen); c != hostfs.ProbeOK {
				fail("unmount " + w.trace.abbrev(pv.GMPath) + ": host is down")
				return
			}
		}
		for h.Mounts.Unmount(pv.GMPath) {
		}
	}
	if d, ok := h.Dirs.Dir(pv.GMPath); ok && d.Entries > 0 {
		fail("remove " + w.trace.abbrev(pv.GMPath) + ": directory not empty")
		return
	}
	h.Dirs.RemoveDir(pv.GMPath)
	v.staged = false
	attempts := v.unstage.failures + 1
	v.unstage.reset()
	v.stage.reset()
	k.trace("unstage %s: ok (attempt %d), globalmount dir removed", pv.Name, attempts)
}

// syncLocal notices local data the healer moved away (Quarantine empties
// the fake directory).
func (k *Kubelet) syncLocal() {
	paths := make([]string, 0, len(k.local))
	for p := range k.local {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if d, ok := k.host().Dirs.Dir(p); !ok || d.Entries == 0 {
			delete(k.local, p)
		}
	}
}

// WriteLocal writes a file into a local (non-mounted) directory, as a pod
// or a human would; the name is recorded as a secret.
func (k *Kubelet) WriteLocal(dir, name string) {
	w := k.n.w
	h := k.host()
	if k.local[dir] == nil {
		k.local[dir] = map[string]bool{}
		if d, ok := h.Dirs.Dir(dir); ok && d.Entries > 0 {
			// Unknown content written outside the model: keep its count.
			for i := 0; i < d.Entries; i++ {
				k.local[dir][fmt.Sprintf("%sunknown-%d", SecretPrefix, i)] = true
			}
		}
	}
	if k.local[dir][name] {
		return
	}
	k.local[dir][name] = true
	w.addSecret(name)
	n := len(k.local[dir])
	h.Dirs.SetDir(dir, fake.Dir{Entries: n, Files: int64(n), Bytes: int64(64 * n), ModTime: w.Now()})
}

// writeHeartbeats lets every running pod write hb-<pod> into each volume
// every tick: into samba through a live cifs mount, into the globalmount
// dir through a bare bind, into the pod dir when nothing is mounted; a
// dead cifs mount swallows the write (EIO).
func (k *Kubelet) writeHeartbeats(pods []corev1.Pod) {
	w := k.n.w
	snap, _ := k.host().Mounts.Snapshot(w.ctx)
	for i := range pods {
		p := &pods[i]
		ps := k.pods[p.UID]
		if ps == nil || !ps.running || p.DeletionTimestamp != nil {
			continue
		}
		for _, pv := range ps.pvs {
			path := w.layout.PodVolumeMountPath(p.UID, pv.Name)
			top, ok := snap.Lookup(path)
			switch {
			case !ok:
				k.WriteLocal(path, SecretPrefix+"hb-"+p.Name)
			case k.bareBinds[top.ID] != "":
				k.WriteLocal(k.bareBinds[top.ID], SecretPrefix+"hb-"+p.Name)
			default:
				s, ok := k.sessions[top.ID]
				if !ok {
					continue
				}
				if c, _ := w.Samba.classify(s.server, s.gen); c == hostfs.ProbeOK {
					w.Samba.write(s.server, pv.Share, "hb-"+p.Name)
				}
			}
		}
	}
}

// sweepOrphans removes empty, unmounted pod volume dirs of pods kubelet no
// longer knows (cleanupOrphanedPodDirs).
func (k *Kubelet) sweepOrphans(present map[types.UID]bool) {
	w := k.n.w
	h := k.host()
	dirs, _ := h.Dirs.ListPodVolumeDirs(w.ctx)
	for _, d := range dirs {
		if present[d.PodUID] || k.vol(d.PVName).pods[d.PodUID] || h.Mounts.IsMountpoint(d.Path) {
			continue
		}
		if dir, ok := h.Dirs.Dir(d.Path); ok && dir.Entries > 0 {
			continue
		}
		if h.Dirs.RemoveDir(d.Path) {
			k.trace("removed orphaned pod volume dir %s", w.trace.abbrev(d.Path))
		}
	}
}

// updateProbeResults feeds the samba state of every cifs mount into the
// fake prober (structural checks need no help: the prober reads the
// snapshot for not_mountpoint and wrong_fstype itself).
func (k *Kubelet) updateProbeResults() {
	w := k.n.w
	h := k.host()
	snap, _ := h.Mounts.Snapshot(w.ctx)
	seen := map[string]bool{}
	for _, e := range snap.Entries {
		top, _ := snap.Lookup(e.MountPoint)
		if seen[e.MountPoint] {
			continue
		}
		seen[e.MountPoint] = true
		class, hung := hostfs.ProbeOK, false
		if s, ok := k.sessions[top.ID]; ok {
			class, hung = w.Samba.classify(s.server, s.gen)
		}
		k.setProbe(e.MountPoint, class, hung)
	}
	for path := range k.stickies {
		if !seen[path] {
			k.setProbe(path, hostfs.ProbeOK, false)
		}
	}
}

func (k *Kubelet) setProbe(path string, class hostfs.ProbeClass, hung bool) {
	p := k.host().Prober
	p.SetHung(path, hung)
	if hung {
		class = hostfs.ProbeHung
	}
	p.SetResult(path, class, "sim: "+string(class))
	if class == hostfs.ProbeOK {
		delete(k.stickies, path)
	} else {
		k.stickies[path] = true
	}
}

func (k *Kubelet) topMount(path string) (hostfs.MountEntry, bool) {
	snap, _ := k.host().Mounts.Snapshot(k.n.w.ctx)
	return snap.Lookup(path)
}

// MountSession registers a cifs mount made outside kubelet (chaos).
func (k *Kubelet) MountSession(path string, server, source string) {
	e := k.host().Mounts.Mount(path, "cifs", source, cifsOptions)
	k.sessions[e.ID] = session{server: server, gen: k.n.w.Samba.Gen(server)}
}

// Staged reports kubelet's view of a volume.
func (k *Kubelet) Staged(pv string) bool { return k.vol(pv).staged }
