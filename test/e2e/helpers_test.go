//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// Stage paths (StageTrace, creation first).
var (
	workloadPath = []v1alpha1.Stage{
		v1alpha1.StagePending, v1alpha1.StagePlanning, v1alpha1.StageDraining, v1alpha1.StageReleasingMounts,
		v1alpha1.StageAwaitingUnstage, v1alpha1.StageQuarantining, v1alpha1.StageAwaitingUnstageCompletion,
		v1alpha1.StageRestartingDriver, v1alpha1.StageScalingUp, v1alpha1.StageVerifying,
		v1alpha1.StageFinalizing, v1alpha1.StageDone,
	}
	sweepPath = []v1alpha1.Stage{
		v1alpha1.StagePlanning, v1alpha1.StageSweeping, v1alpha1.StageAwaitingUnstageCompletion,
		v1alpha1.StageFinalizing, v1alpha1.StageDone,
	}
)

// healExpect describes a healing scenario's expected end state.
type healExpect struct {
	Type v1alpha1.RemediationType
	// Trace is the exact expected stage path.
	Trace []v1alpha1.Stage
	// Workloads are the fixture workloads expected in scope (exactly).
	Workloads []string
	// PVs must all be in scope.PVs.
	PVs []string
	// BrokenPVs are the SmbMounts (agent node) that must be seen Broken
	// (top level or on a pod mount) and end Healthy with LAST OUTCOME=healed.
	BrokenPVs []string
}

// checkHealed asserts the plan's "checked after each healing scenario" list
// for remediation r (already finished with outcome healed) that started
// after injectedAt: stage trace, scope, host tasks, SmbMount path
// (Broken -> STAGE shown -> Healthy, LAST OUTCOME healed), new pods with
// cifs mounts, heartbeats reaching samba, replicas restored, events.
func checkHealed(f *framework.F, rec *framework.Recorder, r *v1alpha1.SmbMountRemediation, want healExpect, injectedAt time.Time) {
	f.T.Helper()
	g := f.G
	g.Expect(r.Status.Outcome).To(gomega.Equal(v1alpha1.OutcomeHealed))
	g.Expect(r.Spec.Type).To(gomega.Equal(want.Type), "remediation type")
	g.Expect(r.Spec.NodeName).To(gomega.Equal(f.AgentNode))
	trace := framework.StageTraceOf(r)
	f.Logf("remediation %s trace: %s", r.Name, framework.FormatTrace(trace))
	g.Expect(trace).To(gomega.Equal(want.Trace), "stage trace of %s: %s", r.Name, framework.FormatTrace(trace))

	// Scope.
	g.Expect(r.Status.Scope).NotTo(gomega.BeNil(), "scope")
	var scoped []string
	for _, w := range r.Status.Scope.Workloads {
		scoped = append(scoped, w.Name)
		g.Expect(w.Namespace).To(gomega.Equal(framework.FixturesNamespace))
		g.Expect(w.OriginalReplicas).To(gomega.Equal(f.Fixtures.Workload(w.Name).Replicas), "originalReplicas of %s", w.Name)
	}
	g.Expect(scoped).To(gomega.ConsistOf(stringsToAny(want.Workloads)...), "scoped workloads")
	for _, pv := range want.PVs {
		g.Expect(r.Status.Scope.PVs).To(gomega.ContainElement(pv), "scope.pvs")
	}

	// Host tasks reached Succeeded.
	tasks := f.HostTasksOf(r.Name)
	g.Expect(tasks).NotTo(gomega.BeEmpty(), "host tasks of %s", r.Name)
	for _, t := range tasks {
		g.Expect(t.Status.Phase).To(gomega.Equal(v1alpha1.HostTaskSucceeded), "host task %s results %+v", t.Name, t.Status.Results)
	}
	for _, ref := range r.Status.HostTasks {
		g.Expect(ref.Phase).To(gomega.Equal(v1alpha1.HostTaskSucceeded), "status.hostTasks %s", ref.Name)
	}

	// SmbMount path.
	for _, pv := range want.BrokenPVs {
		name := v1alpha1.SmbMountName(f.AgentNode, pv)
		g.Expect(rec.SawHealth(name, v1alpha1.HealthBroken, true)).To(gomega.BeTrue(),
			"smbm %s never seen Broken:\n%s", name, rec.Describe(name))
		g.Expect(rec.Stages(name, r.Name)).NotTo(gomega.BeEmpty(),
			"smbm %s never showed STAGE of %s:\n%s", name, r.Name, rec.Describe(name))
		g.Eventually(func(g gomega.Gomega) {
			m := f.SmbMount(f.AgentNode, pv)
			g.Expect(m).NotTo(gomega.BeNil())
			g.Expect(m.Status.Health).To(gomega.Equal(v1alpha1.HealthHealthy))
			g.Expect(m.Status.LastRemediation).NotTo(gomega.BeNil())
			g.Expect(m.Status.LastRemediation.Name).To(gomega.Equal(r.Name))
			g.Expect(m.Status.LastRemediation.Outcome).To(gomega.Equal(v1alpha1.OutcomeHealed))
			g.Expect(m.Status.Remediation == nil || !m.Status.Remediation.Active).To(gomega.BeTrue(), "STAGE cleared")
		}).Should(gomega.Succeed(), "smbm %s healed:\n%s", name, rec.Describe(name))
		f.Logf("smbm %s healths %v, stages %v", name, rec.Healths(name), rec.Stages(name, r.Name))
		// MountRecovered is per mount path (Broken -> Healthy on the same
		// path); a remediated pod mount is replaced, not recovered.
		g.Expect(f.EventReasons("SmbMount", name, injectedAt)).To(
			gomega.ContainElement("Warning/MountBroken"), "events of smbm %s", name)
	}

	// Workloads: replicas restored, new pods with cifs mounts, heartbeats.
	for _, w := range want.Workloads {
		checkWorkloadHealthy(f, w, r.CreationTimestamp.Time)
	}

	// Remediation events.
	g.Expect(f.EventReasons("SmbMountRemediation", r.Name, injectedAt)).To(
		gomega.ContainElement("Normal/RemediationFinished"), "events of %s", r.Name)
}

// checkWorkloadHealthy waits until workload w runs its baseline replicas
// with pods created after `after`, each pod's volumes cifs-mounted (worker
// view and node ground truth) and Healthy, and its heartbeat reaching samba.
func checkWorkloadHealthy(f *framework.F, w string, after time.Time) {
	f.T.Helper()
	wl := f.Fixtures.Workload(w)
	f.G.Eventually(func(g gomega.Gomega) {
		spec, ready, err := f.Fixtures.Replicas(w)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(spec).To(gomega.Equal(wl.Replicas), "spec replicas of %s", w)
		g.Expect(ready).To(gomega.Equal(wl.Replicas), "ready replicas of %s", w)
		pods := f.Fixtures.Pods(w)
		g.Expect(pods).To(gomega.HaveLen(int(wl.Replicas)))
		for _, p := range pods {
			g.Expect(p.CreationTimestamp.Time).To(gomega.BeTemporally(">=", after.Add(-time.Second)), "pod %s is new", p.Name)
			for _, v := range wl.Volumes {
				m := f.SmbMount(f.AgentNode, v.PV)
				g.Expect(m).NotTo(gomega.BeNil(), "smbm %s", v.PV)
				pm := podMount(m, p.UID)
				g.Expect(pm).NotTo(gomega.BeNil(), "pod %s in smbm %s podMounts", p.Name, m.Name)
				g.Expect(pm.FSType).To(gomega.Equal("cifs"), "pod %s %s fstype", p.Name, v.PV)
				g.Expect(pm.Health).To(gomega.Equal(v1alpha1.HealthHealthy), "pod %s %s health", p.Name, v.PV)
				fstype, _ := f.Chaos.MountInfo(f.AgentNode, f.Fixtures.PodVolumePath(p.UID, v.PV))
				g.Expect(fstype).To(gomega.Equal("cifs"), "findmnt pod %s %s", p.Name, v.PV)
			}
		}
		hb, err := f.Fixtures.Heartbeat(w)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		for k, v := range hb {
			g.Expect(epoch(v)).To(gomega.BeNumerically(">=", after.Unix()), "heartbeat %s written after the fix", k)
		}
	}).WithTimeout(3*time.Minute).Should(gomega.Succeed(), "workload %s healthy", w)
}

func podMount(m *v1alpha1.SmbMount, uid any) *v1alpha1.PodMountStatus {
	for i := range m.Status.PodMounts {
		if string(m.Status.PodMounts[i].UID) == fmt.Sprint(uid) {
			return &m.Status.PodMounts[i]
		}
	}
	return nil
}

func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// onlyRemediation asserts exactly one remediation was created during the
// test and returns it.
func onlyRemediation(f *framework.F) *v1alpha1.SmbMountRemediation {
	f.T.Helper()
	rs := f.NewRemediations()
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, r.Name+"("+string(r.Spec.Type)+","+string(r.Status.Outcome)+")")
	}
	f.G.Expect(rs).To(gomega.HaveLen(1), "new remediations: %v", names)
	return &rs[0]
}

// injectWrongFSType is the incident-1 injection: lazily detach pv's
// globalmount and recreate the pods of workload while the volume stays in
// use, so kubelet keeps it staged and bind-mounts the bare local
// globalmount directory into the replacement pod (wrong_fstype).
//
// "In use" is a bare pinner pod on the same PV, created before and deleted
// right after the replacement pod got its (bad) mount - well within
// POD_STARTUP_GRACE, before the worker first probes the new pod. Without
// it kubelet may unstage the volume between the old pod's exit and the new
// pod's start (or retry NodeStage after a failed unstage) and stage it
// cleanly again, and nothing breaks. In production the overlap comes from
// rolling updates and multi-replica workloads.
//
// If shadow is set, a file of that name is also written into the bare
// globalmount directory (incident 2's shadow data); it makes kubelet's
// unstage fail until the remediation quarantined it.
func injectWrongFSType(f *framework.F, pv, workload, shadow string) time.Time {
	f.T.Helper()
	at := time.Now()
	pin := "pin-" + strings.TrimPrefix(pv, "storagebox-e2e-")
	f.Logf("inject: pin %s with bare pod %s, umount -l its globalmount, recreate pods of %s", pv, pin, workload)
	f.StartBarePod(pin, pv)
	f.Fixtures.DetachGlobalmount(pv)
	if shadow != "" {
		f.Chaos.WriteFile(f.AgentNode, f.Fixtures.GlobalmountPath(pv)+"/"+shadow, "shadow data")
	}
	old := f.Fixtures.DeletePod(workload)
	f.G.Eventually(func(g gomega.Gomega) {
		pods := f.Fixtures.Pods(workload)
		g.Expect(pods).NotTo(gomega.BeEmpty())
		for _, p := range pods {
			g.Expect(old).NotTo(gomega.ContainElement(p.UID), "pod %s is a replacement", p.Name)
			fstype, _ := f.Chaos.MountInfo(f.AgentNode, f.Fixtures.PodVolumePath(p.UID, pv))
			g.Expect(fstype).NotTo(gomega.BeEmpty(), "pod %s has its %s mount", p.Name, pv)
			g.Expect(fstype).NotTo(gomega.Equal("cifs"), "pod %s got the bare globalmount dir", p.Name)
			f.Logf("inject: pod %s mounted %s with fstype %s", p.Name, pv, fstype)
		}
	}).WithPolling(500*time.Millisecond).Should(gomega.Succeed(), "replacement pod of %s bind-mounts the bare globalmount", workload)
	f.DeleteBarePod(pin)
	return at
}

// checkQuarantined asserts that remediation r quarantined pv's data into an
// SmbQuarantine with the given file count, remediationRef r, and the
// ShadowDataPending condition on the SmbMount. Returns the SmbQuarantine.
func checkQuarantined(f *framework.F, r *v1alpha1.SmbMountRemediation, pv string, files int64) *v1alpha1.SmbQuarantine {
	f.T.Helper()
	var q *v1alpha1.SmbQuarantine
	f.G.Eventually(func(g gomega.Gomega) {
		q = nil
		for _, x := range f.Quarantines() {
			if x.Spec.PVName == pv && x.Spec.RemediationRef == r.Name {
				q = x.DeepCopy()
			}
		}
		g.Expect(q).NotTo(gomega.BeNil(), "smbq of %s from %s", pv, r.Name)
		g.Expect(q.Status.Files).To(gomega.Equal(files))
		g.Expect(q.Spec.NodeName).To(gomega.Equal(f.AgentNode))
		g.Expect(q.Spec.Path).To(gomega.HavePrefix(framework.QuarantineRoot + "/" + pv + "-"))
		g.Expect(q.Labels).To(gomega.HaveKeyWithValue(v1alpha1.LabelNode, f.AgentNode))
		m := f.SmbMount(f.AgentNode, pv)
		if m != nil {
			g.Expect(m.Status.PendingQuarantines).To(gomega.BeNumerically(">=", 1), "pendingQuarantines")
		}
	}).Should(gomega.Succeed())
	return q
}

// waitStage watches remediation name until its stage is one of stages (it
// returns the stage seen) or it is past all of them (returns "" plus the
// stage it is in). Uses a watch, so a stage that lasts only milliseconds is
// still seen.
func waitStage(f *framework.F, name string, timeout time.Duration, stages ...v1alpha1.Stage) v1alpha1.Stage {
	f.T.Helper()
	wc, err := client.NewWithWatch(f.Config, client.Options{Scheme: f.Client.Scheme()})
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var l v1alpha1.SmbMountRemediationList
		f.G.Expect(wc.List(f.Ctx, &l)).To(gomega.Succeed())
		for i := range l.Items {
			if l.Items[i].Name == name && slices.Contains(stages, l.Items[i].Status.Stage) {
				return l.Items[i].Status.Stage
			}
		}
		w, err := wc.Watch(f.Ctx, &v1alpha1.SmbMountRemediationList{}, &client.ListOptions{
			Raw: &metav1.ListOptions{ResourceVersion: l.ResourceVersion, FieldSelector: "metadata.name=" + name},
		})
		f.G.Expect(err).NotTo(gomega.HaveOccurred())
		if st, ok := consumeStages(w, deadline, stages); ok {
			return st
		}
	}
	f.T.Fatalf("remediation %s did not reach any of %v within %s", name, stages, timeout)
	return ""
}

func consumeStages(w watch.Interface, deadline time.Time, stages []v1alpha1.Stage) (v1alpha1.Stage, bool) {
	defer w.Stop()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return "", false
		case ev, ok := <-w.ResultChan():
			if !ok {
				return "", false
			}
			r, isRem := ev.Object.(*v1alpha1.SmbMountRemediation)
			if !isRem {
				return "", false
			}
			if slices.Contains(stages, r.Status.Stage) {
				return r.Status.Stage, true
			}
		}
	}
}

// waitHostTask waits (watching) until a host task of remediation rem whose
// name contains part reaches phase, and returns it.
func waitHostTask(f *framework.F, rem, part string, phase v1alpha1.HostTaskPhase, timeout time.Duration) *v1alpha1.SmbHostTask {
	f.T.Helper()
	wc, err := client.NewWithWatch(f.Config, client.Options{Scheme: f.Client.Scheme()})
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	match := func(t *v1alpha1.SmbHostTask) bool {
		return t.Labels[v1alpha1.LabelRemediation] == rem && strings.Contains(t.Name, part) && t.Status.Phase == phase
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var l v1alpha1.SmbHostTaskList
		f.G.Expect(wc.List(f.Ctx, &l)).To(gomega.Succeed())
		for i := range l.Items {
			if match(&l.Items[i]) {
				return l.Items[i].DeepCopy()
			}
		}
		w, err := wc.Watch(f.Ctx, &v1alpha1.SmbHostTaskList{}, &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: l.ResourceVersion}})
		f.G.Expect(err).NotTo(gomega.HaveOccurred())
		if t := consumeTasks(w, deadline, match); t != nil {
			return t
		}
	}
	f.T.Fatalf("no host task %q of %s reached %s within %s", part, rem, phase, timeout)
	return nil
}

func consumeTasks(w watch.Interface, deadline time.Time, match func(*v1alpha1.SmbHostTask) bool) *v1alpha1.SmbHostTask {
	defer w.Stop()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			ht, isTask := ev.Object.(*v1alpha1.SmbHostTask)
			if !isTask {
				return nil
			}
			if match(ht) {
				return ht.DeepCopy()
			}
		}
	}
}

// currentStage reads the stage of remediation name.
func currentStage(f *framework.F, name string) v1alpha1.Stage {
	f.T.Helper()
	var r v1alpha1.SmbMountRemediation
	f.G.Expect(f.Client.Get(f.Ctx, client.ObjectKey{Name: name}, &r)).To(gomega.Succeed())
	return r.Status.Stage
}

// hostResults collects the results of every host task of rem by action type.
func hostResults(f *framework.F, rem string) map[v1alpha1.HostActionType][]v1alpha1.HostActionResult {
	f.T.Helper()
	out := map[v1alpha1.HostActionType][]v1alpha1.HostActionResult{}
	for _, t := range f.HostTasksOf(rem) {
		types := map[string]v1alpha1.HostActionType{}
		for _, a := range t.Spec.Actions {
			types[a.ID] = a.Type
		}
		for _, res := range t.Status.Results {
			out[types[res.ID]] = append(out[types[res.ID]], res)
		}
	}
	return out
}

// workerPod is the worker on the agent node.
func workerPod(f *framework.F) *corev1.Pod {
	f.T.Helper()
	return f.HealerPod("worker")
}
