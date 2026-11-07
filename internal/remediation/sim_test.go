package remediation

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

const (
	testNode    = "n1"
	kubeletRoot = "/var/lib/kubelet"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func testCfg() Config {
	return Config{
		PollInterval:          3 * time.Second,
		PlanningProbeWait:     15 * time.Second,
		ScaleDownWait:         120 * time.Second,
		UnstageWait:           30 * time.Second,
		QuarantineUnstageWait: 300 * time.Second,
		DriverRestartWait:     90 * time.Second,
		VerifyWait:            90 * time.Second,
		VerifyNoPodGrace:      30 * time.Second,
		EpisodeMaxDuration:    30 * time.Minute,
		BackoffBase:           5 * time.Minute,
		BackoffMax:            time.Hour,
	}
}

func gmPath(pv string) string {
	return kubeletRoot + "/plugins/kubernetes.io/csi/smb.csi.k8s.io/hash-" + pv + "/globalmount"
}

func podPath(uid types.UID, pv string) string {
	return kubeletRoot + "/pods/" + string(uid) + "/volumes/kubernetes.io~csi/" + pv + "/mount"
}

func dep(name string) v1alpha1.WorkloadRef {
	return v1alpha1.WorkloadRef{Kind: v1alpha1.WorkloadKindDeployment, Namespace: "media", Name: name}
}

// world is a tiny simulated cluster + node, enough to drive Step.
type world struct {
	now        time.Time
	mounts     map[string]*v1alpha1.SmbMount // by pv
	pods       []PodObservation
	workloads  map[string]*WorkloadObservation
	templates  map[string][]string // workload -> PVs of its pods
	driver     []DriverPodObservation
	tasks      map[string]*v1alpha1.SmbHostTask
	others     []v1alpha1.SmbMountRemediation
	aborted    bool
	deleting   bool
	seq        int
	taskPhase  v1alpha1.HostTaskPhase // phase new tasks get (default Succeeded)
	autoDrain  bool                   // scale 0 removes pods
	spawn      bool                   // scale up creates pods
	newHealth  v1alpha1.Health        // podMount health of spawned pods
	autoDriver bool                   // deleting the driver pod creates a ready replacement
	stuck      bool                   // kubelet never finishes unstage
	refill     bool                   // new shadow data lands in the globalmount dir right after a quarantine
	created    []command.CreateHostTask
}

// newWorld: Deployment app (2 replicas) has pod app-1 on pv-a (broken, dead
// globalmount) and pv-b (healthy). Deployment co has pod co-1 on pv-a
// (co-tenant). Deployment other has pod other-1 on pv-b only.
func newWorld() *world {
	w := &world{
		now: t0, mounts: map[string]*v1alpha1.SmbMount{}, workloads: map[string]*WorkloadObservation{},
		templates: map[string][]string{}, tasks: map[string]*v1alpha1.SmbHostTask{},
		taskPhase: v1alpha1.HostTaskSucceeded, autoDrain: true, spawn: true, newHealth: v1alpha1.HealthHealthy,
		autoDriver: true,
	}
	w.addMount("pv-a", v1alpha1.HealthBroken, &v1alpha1.GlobalmountStatus{Mounted: true, FSType: "cifs", DirExists: true})
	w.addMount("pv-b", v1alpha1.HealthHealthy, &v1alpha1.GlobalmountStatus{Mounted: true, FSType: "cifs", DirExists: true})
	w.addWorkload("app", 2, "pv-a", "pv-b")
	w.addWorkload("co", 1, "pv-a")
	w.addWorkload("other", 1, "pv-b")
	w.addPod("app", "app-1", "u-app-1", map[string]v1alpha1.Health{"pv-a": v1alpha1.HealthBroken, "pv-b": v1alpha1.HealthHealthy})
	w.addPod("co", "co-1", "u-co-1", map[string]v1alpha1.Health{"pv-a": v1alpha1.HealthHealthy})
	w.addPod("other", "other-1", "u-other-1", map[string]v1alpha1.Health{"pv-b": v1alpha1.HealthHealthy})
	w.driver = []DriverPodObservation{{Namespace: "kube-system", Name: "csi-smb-node-a", UID: "drv-1", Running: true, Ready: true}}
	return w
}

func (w *world) addMount(pv string, h v1alpha1.Health, g *v1alpha1.GlobalmountStatus) *v1alpha1.SmbMount {
	sm := &v1alpha1.SmbMount{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SmbMountName(testNode, pv)},
		Spec: v1alpha1.SmbMountSpec{NodeName: testNode, PVName: pv, Share: "//s/" + pv, VolumeHandle: pv,
			GlobalmountPath: gmPath(pv)},
		Status: v1alpha1.SmbMountStatus{Health: h, Globalmount: g},
	}
	w.mounts[pv] = sm
	return sm
}

func (w *world) addWorkload(name string, replicas int32, pvs ...string) {
	ref := dep(name)
	w.workloads[ref.String()] = &WorkloadObservation{Ref: ref, Exists: true, Replicas: replicas, ReadyReplicas: replicas}
	w.templates[ref.String()] = pvs
}

func (w *world) addPod(owner, name string, uid types.UID, health map[string]v1alpha1.Health) {
	var pvs []string
	for pv := range health {
		pvs = append(pvs, pv)
	}
	sort.Strings(pvs)
	var o *v1alpha1.WorkloadRef
	if owner != "" {
		r := dep(owner)
		o = &r
	}
	w.pods = append(w.pods, PodObservation{Namespace: "media", Name: name, UID: uid, NodeName: testNode, PVs: pvs,
		Owner: o, Phase: corev1.PodRunning})
	for _, pv := range pvs {
		sm := w.mounts[pv]
		r := v1alpha1.FailureReason("")
		if health[pv] == v1alpha1.HealthBroken {
			r = v1alpha1.ReasonWrongFSType
		}
		sm.Status.PodMounts = append(sm.Status.PodMounts, v1alpha1.PodMountStatus{
			Namespace: "media", Pod: name, UID: uid, Mounted: true, FSType: "cifs", Health: health[pv], Reason: r,
		})
	}
}

func (w *world) removePod(uid types.UID) {
	var keep []PodObservation
	for _, p := range w.pods {
		if p.UID != uid {
			keep = append(keep, p)
		}
	}
	w.pods = keep
	for _, sm := range w.mounts {
		var pms []v1alpha1.PodMountStatus
		for _, pm := range sm.Status.PodMounts {
			if pm.UID != uid {
				pms = append(pms, pm)
			}
		}
		sm.Status.PodMounts = pms
	}
}

func (w *world) obs() Observation {
	o := Observation{Now: w.now, Aborted: w.aborted, Deleting: w.deleting}
	pvs := make([]string, 0, len(w.mounts))
	for pv := range w.mounts {
		pvs = append(pvs, pv)
	}
	sort.Strings(pvs)
	for _, pv := range pvs {
		o.SmbMounts = append(o.SmbMounts, *w.mounts[pv].DeepCopy())
	}
	for _, p := range w.pods {
		p.PVs = append([]string(nil), p.PVs...)
		o.Pods = append(o.Pods, p)
	}
	ks := make([]string, 0, len(w.workloads))
	for k := range w.workloads {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		o.Workloads = append(o.Workloads, *w.workloads[k])
	}
	o.DriverPods = append(o.DriverPods, w.driver...)
	tn := make([]string, 0, len(w.tasks))
	for n := range w.tasks {
		tn = append(tn, n)
	}
	sort.Strings(tn)
	for _, n := range tn {
		o.HostTasks = append(o.HostTasks, *w.tasks[n].DeepCopy())
	}
	o.OtherActive = append(o.OtherActive, w.others...)
	return o
}

func (w *world) usedPVs() map[string]bool {
	out := map[string]bool{}
	for _, p := range w.pods {
		for _, pv := range p.PVs {
			out[pv] = true
		}
	}
	return out
}

func (w *world) mountByPath(path string) *v1alpha1.SmbMount {
	for _, sm := range w.mounts {
		if sm.Spec.GlobalmountPath == path {
			return sm
		}
	}
	return nil
}

// apply executes commands against the world, then lets "kubelet" act.
func (w *world) apply(cmds []command.ClusterCommand) {
	for _, c := range cmds {
		switch c := c.(type) {
		case command.ScaleWorkload:
			wo := w.workloads[c.Workload.String()]
			if wo == nil {
				continue
			}
			wo.Replicas = c.Replicas
			if c.Replicas == 0 && w.autoDrain {
				for _, p := range append([]PodObservation(nil), w.pods...) {
					if p.Owner != nil && *p.Owner == c.Workload {
						w.removePod(p.UID)
					}
				}
			}
			if c.Replicas > 0 && w.spawn {
				for i := int32(0); i < c.Replicas; i++ {
					w.seq++
					h := map[string]v1alpha1.Health{}
					for _, pv := range w.templates[c.Workload.String()] {
						h[pv] = w.newHealth
						if w.mounts[pv] == nil {
							w.addMount(pv, v1alpha1.HealthHealthy, &v1alpha1.GlobalmountStatus{Mounted: true, DirExists: true})
						}
					}
					w.addPod(c.Workload.Name, fmt.Sprintf("%s-new-%d", c.Workload.Name, w.seq), types.UID(fmt.Sprintf("u-new-%d", w.seq)), h)
				}
			}
		case command.DeletePod:
			var keep []DriverPodObservation
			for _, d := range w.driver {
				if d.UID != c.UID {
					keep = append(keep, d)
				}
			}
			if len(keep) != len(w.driver) && w.autoDriver {
				keep = append(keep, DriverPodObservation{Namespace: "kube-system", Name: "csi-smb-node-b", UID: "drv-2", Running: true, Ready: true})
			}
			w.driver = keep
		case command.CreateHostTask:
			if w.tasks[c.Name] != nil {
				continue
			}
			w.created = append(w.created, c)
			w.tasks[c.Name] = &v1alpha1.SmbHostTask{
				ObjectMeta: metav1.ObjectMeta{Name: c.Name},
				Spec:       v1alpha1.SmbHostTaskSpec{NodeName: c.NodeName, DryRun: c.DryRun, Actions: c.Actions},
				Status:     v1alpha1.SmbHostTaskStatus{Phase: w.taskPhase},
			}
			if c.DryRun || w.taskPhase != v1alpha1.HostTaskSucceeded {
				continue
			}
			for _, a := range c.Actions {
				sm := w.mountByPath(a.Path)
				if sm == nil || sm.Status.Globalmount == nil {
					continue
				}
				switch a.Type {
				case v1alpha1.HostActionForceUnmount:
					if a.OnlyIf == v1alpha1.OnlyIfNotHealthyMount && sm.Status.Globalmount.Mounted &&
						sm.Status.Health == v1alpha1.HealthHealthy {
						continue
					}
					sm.Status.Globalmount.Mounted = false
				case v1alpha1.HostActionQuarantine:
					if !sm.Status.Globalmount.Mounted && !w.refill {
						sm.Status.Globalmount.Empty = true
					}
				case v1alpha1.HostActionProbeNow:
				}
			}
		case command.PatchSmbMountRemediationMirror:
			for _, sm := range w.mounts {
				if sm.Name != c.SmbMount {
					continue
				}
				sm.Status.Remediation = c.Remediation
				sm.Status.LastRemediation = c.LastRemediation
				var keep []metav1.Condition
				for _, cd := range sm.Status.Conditions {
					if cd.Type == v1alpha1.ConditionHealthy || cd.Type == v1alpha1.ConditionShadowDataPending {
						keep = append(keep, cd)
					}
				}
				keep = append(keep, c.Conditions...)
				sm.Status.Conditions = keep
			}
		case command.DeleteHostTask, command.EmitEvent:
		}
	}
	// kubelet: an unreferenced, unmounted, empty globalmount dir is unstaged.
	used := w.usedPVs()
	for pv, sm := range w.mounts {
		g := sm.Status.Globalmount
		if g == nil || used[pv] || w.stuck {
			continue
		}
		if !g.Mounted && g.DirExists && g.Empty {
			g.DirExists = false
		}
	}
}

func newRemediation(typ v1alpha1.RemediationType, mounts ...string) *v1alpha1.SmbMountRemediation {
	return &v1alpha1.SmbMountRemediation{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.RemediationName(testNode, t0), UID: "rem-uid",
			CreationTimestamp: metav1.NewTime(t0)},
		Spec: v1alpha1.SmbMountRemediationSpec{NodeName: testNode, Type: typ,
			Trigger: v1alpha1.RemediationTrigger{SmbMounts: mounts}},
	}
}

func workloadRem() *v1alpha1.SmbMountRemediation {
	return newRemediation(v1alpha1.RemediationTypeWorkload, v1alpha1.SmbMountName(testNode, "pv-a"))
}

// step runs Step, checks the table invariant, stores the status and applies the commands.
func (w *world) step(t *testing.T, obj *v1alpha1.SmbMountRemediation, cfg Config) Result {
	t.Helper()
	before := obj.DeepCopy()
	res := Step(obj, w.obs(), cfg)
	if !equalObj(before, obj) {
		t.Fatalf("Step mutated its input")
	}
	assertInTable(t, obj.Spec.Type, res.Transitions)
	obj.Status = res.Status
	w.apply(res.Commands)
	return res
}

// runUntil steps (advancing 3s per step) until the stage is reached and Step waits there.
func (w *world) runUntil(t *testing.T, obj *v1alpha1.SmbMountRemediation, cfg Config, stage v1alpha1.Stage) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if obj.Status.Stage == stage {
			return
		}
		w.step(t, obj, cfg)
		if obj.Status.Stage == stage {
			return
		}
		w.now = w.now.Add(3 * time.Second)
	}
	t.Fatalf("never reached %s (at %s); trace %s", stage, obj.Status.Stage, trace(obj))
}

func trace(obj *v1alpha1.SmbMountRemediation) string {
	var b strings.Builder
	for _, tr := range obj.Status.Transitions {
		fmt.Fprintf(&b, "%s->%s(%s) ", tr.From, tr.To, tr.Reason)
	}
	return b.String()
}

func assertInTable(t *testing.T, typ v1alpha1.RemediationType, trs []Transition) {
	t.Helper()
	if typ == "" {
		typ = v1alpha1.RemediationTypeWorkload
	}
	for _, tr := range trs {
		if RuleIndex(typ, tr.From, tr.To, tr.Reason) < 0 {
			t.Fatalf("transition %s -> %s (%s) is not in the %s table", tr.From, tr.To, tr.Reason, typ)
		}
	}
}

func equalObj(a, b *v1alpha1.SmbMountRemediation) bool {
	return reflect.DeepEqual(a, b)
}

func hasTransition(res Result, from, to v1alpha1.Stage, reason string) bool {
	for _, tr := range res.Transitions {
		if tr.From == from && tr.To == to && tr.Reason == reason {
			return true
		}
	}
	return false
}

func cmdsOf[T command.ClusterCommand](cmds []command.ClusterCommand) []T {
	var out []T
	for _, c := range cmds {
		if x, ok := c.(T); ok {
			out = append(out, x)
		}
	}
	return out
}

func keys(cmds []command.ClusterCommand) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, fmt.Sprintf("%T/%s", c, c.Key()))
	}
	return out
}
