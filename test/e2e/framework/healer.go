//go:build e2e

package framework

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// ControlPlaneDeployment is the control-plane Deployment (<fullname>-controlplane).
const ControlPlaneDeployment = "smb-mount-healer-controlplane"

// HealerPod returns the ready healer pod of component (the worker on the
// agent node).
func (f *F) HealerPod(component string) *corev1.Pod {
	f.T.Helper()
	node := ""
	if component == metrics.ComponentWorker {
		node = f.AgentNode
	}
	return f.ReadyPod(HealerNamespace, ComponentLabel+"="+component, node)
}

// KillNow force-deletes the healer pod of component (grace period 0, so a
// frozen process is SIGKILLed right away) and waits for a ready replacement.
func (f *F) KillNow(component string) *corev1.Pod {
	f.T.Helper()
	node := ""
	if component == metrics.ComponentWorker {
		node = f.AgentNode
	}
	sel := ComponentLabel + "=" + component
	ps, err := f.pods(HealerNamespace, sel, node)
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	f.G.Expect(ps).To(gomega.HaveLen(1), "one %s pod on %q", component, node)
	old := ps[0]
	f.G.Expect(f.Kube.CoreV1().Pods(HealerNamespace).Delete(f.Ctx, old.Name,
		metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})).To(gomega.Succeed())
	var p *corev1.Pod
	f.G.Eventually(func(g gomega.Gomega) {
		var err error
		p, err = f.readyPod(HealerNamespace, sel, node)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(p.UID).NotTo(gomega.Equal(old.UID))
	}).WithTimeout(3 * time.Minute).Should(gomega.Succeed())
	return p
}

// PodLogs returns the logs of container in ns/pod since t (all if zero).
func (f *F) PodLogs(ns, pod, container string, since time.Time) string {
	f.T.Helper()
	opts := &corev1.PodLogOptions{Container: container}
	if !since.IsZero() {
		st := metav1.NewTime(since)
		opts.SinceTime = &st
	}
	raw, err := f.Kube.CoreV1().Pods(ns).GetLogs(pod, opts).DoRaw(f.Ctx)
	f.G.Expect(err).NotTo(gomega.HaveOccurred(), "logs of %s/%s", ns, pod)
	return string(raw)
}

// SetControlPlaneDryRun sets DRY_RUN on the control-plane Deployment and
// waits until the rollout finished and a single ready pod runs the new
// template. on=false removes the variable again (values-e2e.yaml keeps the
// chart default dryRun=false, so neither --dry-run nor DRY_RUN is rendered).
func (f *F) SetControlPlaneDryRun(on bool) {
	f.T.Helper()
	f.G.Eventually(func() error {
		d, err := f.Kube.AppsV1().Deployments(HealerNamespace).Get(f.Ctx, ControlPlaneDeployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		c := &d.Spec.Template.Spec.Containers[0]
		c.Env = slices.DeleteFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == "DRY_RUN" })
		if on {
			c.Env = append(c.Env, corev1.EnvVar{Name: "DRY_RUN", Value: "true"})
		}
		_, err = f.Kube.AppsV1().Deployments(HealerNamespace).Update(f.Ctx, d, metav1.UpdateOptions{})
		return err
	}).Should(gomega.Succeed())
	f.G.Eventually(func(g gomega.Gomega) {
		d, err := f.Kube.AppsV1().Deployments(HealerNamespace).Get(f.Ctx, ControlPlaneDeployment, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(d.Status.ObservedGeneration).To(gomega.BeNumerically(">=", d.Generation))
		g.Expect(d.Status.UpdatedReplicas).To(gomega.Equal(int32(1)))
		g.Expect(d.Status.Replicas).To(gomega.Equal(int32(1)), "old pod gone")
		g.Expect(d.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
		p, err := f.readyPod(HealerNamespace, ComponentLabel+"="+metrics.ComponentControlPlane, "")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		has := slices.ContainsFunc(p.Spec.Containers[0].Env, func(e corev1.EnvVar) bool { return e.Name == "DRY_RUN" })
		g.Expect(has).To(gomega.Equal(on))
	}).WithTimeout(3 * time.Minute).Should(gomega.Succeed())
}

// Events lists the events (events.k8s.io/v1, any namespace) regarding the
// object kind/name created at or after since, oldest first.
func (f *F) Events(kind, name string, since time.Time) []eventsv1.Event {
	f.T.Helper()
	l, err := f.Kube.EventsV1().Events("").List(f.Ctx, metav1.ListOptions{})
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	var out []eventsv1.Event
	for _, e := range l.Items {
		if e.Regarding.Kind != kind || e.Regarding.Name != name {
			continue
		}
		if eventTime(&e).Before(since.Add(-time.Second)) {
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b eventsv1.Event) int { return eventTime(&a).Compare(eventTime(&b)) })
	return out
}

// EventReasons are the "<type>/<reason>" of Events(kind, name, since).
func (f *F) EventReasons(kind, name string, since time.Time) []string {
	f.T.Helper()
	var out []string
	for _, e := range f.Events(kind, name, since) {
		out = append(out, e.Type+"/"+e.Reason)
	}
	return out
}

// AllEventsText renders every event (any object) since t, notes included,
// for leak checks.
func (f *F) AllEventsText(since time.Time) string {
	f.T.Helper()
	l, err := f.Kube.EventsV1().Events("").List(f.Ctx, metav1.ListOptions{})
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	var b strings.Builder
	for _, e := range l.Items {
		if eventTime(&e).Before(since.Add(-time.Second)) {
			continue
		}
		raw, _ := json.Marshal(e)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

func eventTime(e *eventsv1.Event) time.Time {
	switch {
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.DeprecatedLastTimestamp.IsZero():
		return e.DeprecatedLastTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// forgetHistory deletes every terminal SmbMountRemediation left by earlier
// tests (logging their outcome and trace first). Terminal remediations are
// the backoff history; without this, scenarios that remediate the same PV
// back to back would be suppressed by backoff (exponential in the number of
// episodes within the last hour).
func (f *F) forgetHistory() {
	f.T.Helper()
	for _, r := range f.Remediations() {
		if !r.Status.Stage.Terminal() {
			continue
		}
		f.Logf("history: deleting terminal remediation %s (%s, outcome %s): %s",
			r.Name, r.Spec.Type, r.Status.Outcome, FormatTrace(StageTraceOf(&r)))
		err := f.Client.Delete(f.Ctx, &r)
		if !apierrors.IsNotFound(err) {
			f.G.Expect(err).NotTo(gomega.HaveOccurred())
		}
	}
	f.G.Eventually(func(g gomega.Gomega) {
		for _, r := range f.Remediations() {
			g.Expect(r.Status.Stage.Terminal()).To(gomega.BeFalse(), "terminal remediation %s still exists", r.Name)
		}
	}).Should(gomega.Succeed())
}

// --- quarantine dir --------------------------------------------------------

// ClearQuarantines registers a cleanup (run before the baseline wait) that
// removes every folder under QuarantineRoot on the agent node and waits until
// no SmbQuarantine is left. For tests that quarantine data on purpose.
func ClearQuarantines() Option {
	return func(f *F) { f.clearQuarantines = true }
}

// RemoveQuarantineFolders deletes every folder under QuarantineRoot on the
// agent node (test data only) and waits for the SmbQuarantines to go.
func (f *F) RemoveQuarantineFolders() {
	f.T.Helper()
	f.Chaos.Sh(f.AgentNode, fmt.Sprintf("if [ -d %[1]s ]; then find %[1]s -mindepth 1 -maxdepth 1 -exec rm -rf {} +; fi", ShellQuote(QuarantineRoot)))
	f.G.Eventually(func(g gomega.Gomega) {
		var l v1alpha1.SmbQuarantineList
		g.Expect(f.Client.List(f.Ctx, &l)).To(gomega.Succeed())
		g.Expect(l.Items).To(gomega.BeEmpty())
	}).Should(gomega.Succeed(), "SmbQuarantines removed after clearing %s", QuarantineRoot)
}

// QuarantineFolders lists the folder names under QuarantineRoot on the agent node.
func (f *F) QuarantineFolders() []string {
	f.T.Helper()
	out := f.Chaos.Sh(f.AgentNode, fmt.Sprintf("ls -1A %s 2>/dev/null || true", ShellQuote(QuarantineRoot)))
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names
}

// --- processes -------------------------------------------------------------

// Freeze sends SIGSTOP to every process of pod's first container (through
// the chaos pod on the same node, which shares the node's PID namespace) and
// registers Thaw as a cleanup. While frozen the process keeps its sockets
// and leases but does nothing.
func (f *F) Freeze(pod *corev1.Pod) {
	f.T.Helper()
	f.signal(pod, "STOP")
	f.T.Cleanup(func() { f.thawQuietly(pod) })
}

// Thaw sends SIGCONT to pod's first container.
func (f *F) Thaw(pod *corev1.Pod) {
	f.T.Helper()
	f.signal(pod, "CONT")
}

func (f *F) thawQuietly(pod *corev1.Pod) {
	cid := containerID(pod)
	if cid == "" {
		return
	}
	_, _ = f.Chaos.Exec(pod.Spec.NodeName, "sh", "-c", pidsScript(cid)+` | xargs -r kill -CONT 2>/dev/null || true`)
}

func (f *F) signal(pod *corev1.Pod, sig string) {
	f.T.Helper()
	cid := containerID(pod)
	f.G.Expect(cid).NotTo(gomega.BeEmpty(), "container id of %s", pod.Name)
	out := f.Chaos.Sh(pod.Spec.NodeName, fmt.Sprintf(`pids=$(%s); [ -n "$pids" ] || { echo "no processes for %s" >&2; exit 1; }; kill -%s $pids; echo $pids`,
		pidsScript(cid), pod.Name, sig))
	f.Logf("sent SIG%s to %s/%s (pids %s)", sig, pod.Namespace, pod.Name, strings.TrimSpace(out))
}

// pidsScript prints the PIDs whose cgroup names container id cid.
func pidsScript(cid string) string {
	return fmt.Sprintf(`for p in /proc/[0-9]*; do grep -qs %s "$p/cgroup" && echo "${p#/proc/}"; done`, ShellQuote(cid))
}

func containerID(pod *corev1.Pod) string {
	if len(pod.Status.ContainerStatuses) == 0 {
		return ""
	}
	id := pod.Status.ContainerStatuses[0].ContainerID
	if i := strings.Index(id, "://"); i >= 0 {
		id = id[i+3:]
	}
	return id
}
