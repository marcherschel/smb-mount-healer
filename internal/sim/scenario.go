package sim

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// Warmup is the untraced start-up time: pods start, outlive
// POD_STARTUP_GRACE and probe healthy.
const Warmup = 2 * time.Minute

// defaultSettle is how long a scenario keeps running once it reached its
// expected outcomes and is healed.
const defaultSettle = 2 * time.Minute

// Scenario is one simulated incident.
type Scenario struct {
	Name string
	// Opts configures the world (nodes, dry run, timings).
	Opts Options
	// Setup adds fixtures before Start.
	Setup func(w *World)
	// Script schedules the injections (hooks) after the warm-up.
	Script func(w *World)
	// Max bounds the simulated time after the warm-up.
	Max time.Duration
	// Expect lists "<Type>:<outcome>" of every remediation, in creation
	// order. Empty: no remediation may be created.
	Expect []string
	// Unhealed: the designed end state is not healed (dry run, unowned pin).
	Unhealed bool
	// Settle overrides defaultSettle.
	Settle time.Duration
	// Check returns extra problems at the end.
	Check func(w *World) []string
}

// Result is the outcome of one run.
type Result struct {
	Trace    []byte
	Problems []string
	Outcomes []string
	Elapsed  time.Duration
}

// Run executes a scenario deterministically.
func Run(s Scenario) (*Result, error) {
	w, err := NewWorld(s.Opts)
	if err != nil {
		return nil, err
	}
	if s.Setup != nil {
		s.Setup(w)
	}
	if err := w.Start(Warmup); err != nil {
		return nil, err
	}
	res := &Result{}
	if ok, why := w.Healed(); !ok {
		res.Problems = append(res.Problems, "baseline not healthy after warm-up: "+why)
	}
	if s.Script != nil {
		s.Script(w)
	}
	settle := s.Settle
	if settle == 0 {
		settle = defaultSettle
	}
	end := w.Now().Add(s.Max)
	for w.Now().Before(end) {
		w.Step()
		if w.done(s) {
			w.RunFor(settle)
			break
		}
	}
	res.Outcomes = w.Outcomes()
	healed, why := w.Healed()
	w.checkCRPrivacy()
	w.trace.add(Event{Kind: "result", Msg: fmt.Sprintf("outcomes=%v healed=%t", res.Outcomes, healed)})
	w.trace.add(Event{Kind: "replicas", Msg: w.replicasSummary()})
	w.trace.add(Event{Kind: "metrics", Msg: w.metricsSummary()})

	if !equalStrings(res.Outcomes, s.Expect) {
		res.Problems = append(res.Problems, fmt.Sprintf("outcomes = %v, want %v", res.Outcomes, s.Expect))
	}
	if !s.Unhealed && !healed {
		res.Problems = append(res.Problems, "not healed at the end: "+why)
	}
	if s.Check != nil {
		res.Problems = append(res.Problems, s.Check(w)...)
	}
	for _, h := range w.hooks {
		if !h.done && h.name != "" {
			res.Problems = append(res.Problems, "scripted step never ran: "+h.name)
		}
	}
	res.Problems = append(res.Problems, w.Violations()...)
	res.Trace = w.trace.Bytes()
	res.Elapsed = w.Elapsed()
	return res, nil
}

// done reports whether every expected remediation finished and the world is
// in its designed end state.
func (w *World) done(s Scenario) bool {
	if len(s.Expect) == 0 || len(w.obs.order) < len(s.Expect) || w.ActiveRemediation() != "" {
		return false
	}
	for _, n := range w.obs.order {
		if w.obs.outcomes[n] == "" {
			return false
		}
	}
	if s.Unhealed {
		return true
	}
	ok, _ := w.Healed()
	return ok
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Healed reports whether the world is healthy: no active remediation, every
// workload at its baseline with all pods Running on cifs mounts, and every
// SmbMount Healthy without dirty orphans.
func (w *World) Healed() (bool, string) {
	if a := w.ActiveRemediation(); a != "" {
		return false, "remediation " + a + " is active"
	}
	pods := w.listPods()
	for _, wl := range w.workloads {
		if wl.Kind != "" {
			if r := wl.Replicas(); r != wl.Baseline {
				return false, fmt.Sprintf("%s has %d replicas, want %d", wl.ref(), r, wl.Baseline)
			}
		}
		running := int32(0)
		for _, p := range wl.Pods() {
			if p.DeletionTimestamp != nil {
				return false, "pod " + p.Name + " is terminating"
			}
			if p.Status.Phase == corev1.PodRunning {
				running++
			}
		}
		if running != wl.Baseline {
			return false, fmt.Sprintf("%s has %d running pods, want %d", wl.ref(), running, wl.Baseline)
		}
	}
	for i := range pods {
		p := &pods[i]
		n := w.nodeByName[p.Spec.NodeName]
		if n == nil || p.Namespace == DriverNS {
			continue
		}
		snap, _ := n.Host.Mounts.Snapshot(w.ctx)
		for _, pv := range w.podPVs(p) {
			e, ok := snap.Lookup(w.layout.PodVolumeMountPath(p.UID, pv.Name))
			if !ok || e.FSType != "cifs" {
				return false, fmt.Sprintf("pod %s volume %s is not a cifs mount", p.Name, pv.Name)
			}
			if c, _ := w.Samba.classify(n.kubelet.sessions[e.ID].server, n.kubelet.sessions[e.ID].gen); c != hostfs.ProbeOK {
				return false, fmt.Sprintf("pod %s volume %s is a dead cifs mount (%s)", p.Name, pv.Name, c)
			}
		}
	}
	for _, sm := range w.smbMounts() {
		st := sm.Status
		for _, pm := range st.PodMounts {
			if pm.Health != v1alpha1.HealthHealthy {
				return false, fmt.Sprintf("SmbMount %s pod %s is %s", sm.Name, pm.Pod, pm.Health)
			}
		}
		if g := st.Globalmount; g != nil && g.Mounted && st.Health != v1alpha1.HealthHealthy {
			return false, fmt.Sprintf("SmbMount %s globalmount is %s", sm.Name, st.Health)
		}
		if g := st.Globalmount; g != nil && !g.Mounted && g.DirExists && !g.Empty {
			return false, "SmbMount " + sm.Name + " globalmount dir holds data"
		}
		for _, od := range st.OrphanPodDirs {
			if od.Mounted || !od.Empty {
				return false, "SmbMount " + sm.Name + " has a dirty orphan pod dir"
			}
		}
	}
	return true, ""
}

func (w *World) replicasSummary() string {
	var parts []string
	for _, wl := range w.workloads {
		running := 0
		for _, p := range wl.Pods() {
			if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
				running++
			}
		}
		parts = append(parts, fmt.Sprintf("%s=%d/%d", wl.ref(), running, wl.Replicas()))
	}
	return strings.Join(parts, " ")
}

// summaryMetrics are the counters and gauges summarised at the end of a trace.
var summaryMetrics = map[string]bool{
	metrics.RemediationsStartedTotal:   true,
	metrics.RemediationsTotal:          true,
	metrics.RemediationsResumedTotal:   true,
	metrics.RemediationSuppressedTotal: true,
	metrics.WorkloadActionsTotal:       true,
	metrics.WorkloadVerificationsTotal: true,
	metrics.HostActionsTotal:           true,
	metrics.FalseAlarmsTotal:           true,
	metrics.ProbesHung:                 true,
	metrics.Quarantines:                true,
}

func (w *World) metricsSummary() string {
	fams, err := w.reg.Gather()
	if err != nil {
		w.violation("gather metrics: %v", err)
		return ""
	}
	var out []string
	for _, f := range fams {
		name := f.GetName()
		if !summaryMetrics[name] {
			continue
		}
		short := strings.TrimPrefix(name, metrics.Namespace+"_")
		for _, m := range f.GetMetric() {
			var v float64
			switch {
			case m.GetCounter() != nil:
				v = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				v = m.GetGauge().GetValue()
			default:
				continue
			}
			if v == 0 && f.GetType() == dto.MetricType_COUNTER {
				continue
			}
			out = append(out, short+labelString(m.GetLabel())+"="+strconv.FormatFloat(v, 'f', -1, 64))
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func labelString(ls []*dto.LabelPair) string {
	var parts []string
	for _, l := range ls {
		if l.GetName() == metrics.LabelComponent {
			continue
		}
		parts = append(parts, l.GetName()+"="+l.GetValue())
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// --- chaos and human actions ---------------------------------------------------

// DetachGlobalmount lazily unmounts a PV's globalmount behind kubelet's back
// (umount -l): kubelet still considers the volume staged.
func (w *World) DetachGlobalmount(node, pv string) {
	n := w.nodeByName[node]
	if !n.Host.Mounts.Unmount(w.pvs[pv].GMPath) {
		w.violation("detach %s on %s: not mounted", pv, node)
	}
}

// WriteGlobalmountDir writes a file into the (unmounted) globalmount dir.
func (w *World) WriteGlobalmountDir(node, pv, file string) {
	w.nodeByName[node].kubelet.WriteLocal(w.pvs[pv].GMPath, SecretPrefix+file)
}

// SeedOrphanPodDir creates a pod volume dir of a pod that does not exist,
// holding one file.
func (w *World) SeedOrphanPodDir(node, uid, pv, file string) {
	path := w.layout.PodVolumeMountPath(typesUID(uid), pv)
	w.podNames[typesUID(uid)] = "orphan-" + uid
	w.nodeByName[node].kubelet.WriteLocal(path, SecretPrefix+file)
}

// MountForeign cifs-mounts server/share at path outside kubelet (chaos);
// the directory is created if missing.
func (w *World) MountForeign(node, path, server, share string) {
	n := w.nodeByName[node]
	if _, ok := n.Host.Dirs.Dir(path); !ok {
		n.Host.Dirs.SetDir(path, fakeDir())
	}
	n.kubelet.MountSession(path, server, "//"+server+"/"+share)
}

// UnknownGlobalmountPath is a globalmount path whose hash matches no PV.
func (w *World) UnknownGlobalmountPath() string {
	return w.layout.StagingRoot() + "/" + kubelet.GlobalmountHash("not-a-pv-of-this-cluster") + "/globalmount"
}

// DeleteRemediation deletes the active remediation (kubectl delete smbr).
func (w *World) DeleteRemediation() {
	name := w.ActiveRemediation()
	r := &v1alpha1.SmbMountRemediation{}
	if err := w.store.Get(w.ctx, client.ObjectKey{Name: name}, r); err != nil {
		w.violation("get remediation: %v", err)
		return
	}
	if err := w.store.Delete(w.ctx, r); err != nil {
		w.violation("delete remediation: %v", err)
	}
}

// AbortRemediation sets the abort annotation on the active remediation.
func (w *World) AbortRemediation() {
	name := w.ActiveRemediation()
	r := &v1alpha1.SmbMountRemediation{}
	if err := w.store.Get(w.ctx, client.ObjectKey{Name: name}, r); err != nil {
		w.violation("get remediation: %v", err)
		return
	}
	if r.Annotations == nil {
		r.Annotations = map[string]string{}
	}
	r.Annotations[v1alpha1.AnnotationAbort] = "true"
	if err := w.store.Update(w.ctx, r); err != nil {
		w.violation("abort remediation: %v", err)
	}
}

// Quarantines returns the SmbQuarantines, sorted by name.
func (w *World) Quarantines() []v1alpha1.SmbQuarantine {
	var list v1alpha1.SmbQuarantineList
	if err := w.store.List(w.ctx, &list); err != nil {
		w.violation("list quarantines: %v", err)
		return nil
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items
}

// DeleteQuarantineObject deletes an SmbQuarantine (kubectl delete smbq);
// the worker's informer sees the delete and rescans.
func (w *World) DeleteQuarantineObject(name string) {
	q := &v1alpha1.SmbQuarantine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := w.store.Delete(w.ctx, q); err != nil {
		w.violation("delete smbq: %v", err)
	}
	for _, n := range w.Nodes {
		n.scanRequested = true
	}
}
