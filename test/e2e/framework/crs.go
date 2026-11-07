//go:build e2e

package framework

import (
	"fmt"
	"sort"
	"strings"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// SmbMounts lists every SmbMount (cluster-scoped), sorted by name.
func (f *F) SmbMounts() []v1alpha1.SmbMount {
	f.T.Helper()
	var l v1alpha1.SmbMountList
	f.G.Expect(f.Client.List(f.Ctx, &l)).To(gomega.Succeed())
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items
}

// SmbMount fetches the SmbMount for (node, pv), or nil if it does not exist.
func (f *F) SmbMount(node, pv string) *v1alpha1.SmbMount {
	f.T.Helper()
	var m v1alpha1.SmbMount
	err := f.Client.Get(f.Ctx, keyOf(v1alpha1.SmbMountName(node, pv)), &m)
	if apierrors.IsNotFound(err) {
		return nil
	}
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	return &m
}

// Remediations lists every SmbMountRemediation, oldest first.
func (f *F) Remediations() []v1alpha1.SmbMountRemediation {
	f.T.Helper()
	var l v1alpha1.SmbMountRemediationList
	f.G.Expect(f.Client.List(f.Ctx, &l)).To(gomega.Succeed())
	sort.Slice(l.Items, func(i, j int) bool {
		a, b := l.Items[i].CreationTimestamp, l.Items[j].CreationTimestamp
		if a.Equal(&b) {
			return l.Items[i].Name < l.Items[j].Name
		}
		return a.Before(&b)
	})
	return l.Items
}

// NewRemediations are the remediations created since New.
func (f *F) NewRemediations() []v1alpha1.SmbMountRemediation {
	f.T.Helper()
	var out []v1alpha1.SmbMountRemediation
	for _, r := range f.Remediations() {
		if !f.remediationsAtStart[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

// HostTasks lists every SmbHostTask, sorted by name.
func (f *F) HostTasks() []v1alpha1.SmbHostTask {
	f.T.Helper()
	var l v1alpha1.SmbHostTaskList
	f.G.Expect(f.Client.List(f.Ctx, &l)).To(gomega.Succeed())
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items
}

// HostTasksOf lists the SmbHostTasks of a remediation (label smbhealer.herschel.io/remediation).
func (f *F) HostTasksOf(remediation string) []v1alpha1.SmbHostTask {
	f.T.Helper()
	var out []v1alpha1.SmbHostTask
	for _, t := range f.HostTasks() {
		if t.Labels[v1alpha1.LabelRemediation] == remediation || ownedBy(t.OwnerReferences, remediation) {
			out = append(out, t)
		}
	}
	return out
}

// Quarantines lists every SmbQuarantine, sorted by name.
func (f *F) Quarantines() []v1alpha1.SmbQuarantine {
	f.T.Helper()
	var l v1alpha1.SmbQuarantineList
	f.G.Expect(f.Client.List(f.Ctx, &l)).To(gomega.Succeed())
	sort.Slice(l.Items, func(i, j int) bool { return l.Items[i].Name < l.Items[j].Name })
	return l.Items
}

// WaitRemediation waits for a remediation created after New to reach the
// terminal stage and asserts its outcome. It fails fast if a new remediation
// finishes with a different outcome. Returns the finished remediation.
func (f *F) WaitRemediation(outcome v1alpha1.Outcome) *v1alpha1.SmbMountRemediation {
	f.T.Helper()
	var done *v1alpha1.SmbMountRemediation
	f.G.Eventually(func(g gomega.Gomega) {
		var pending []string
		for _, r := range f.NewRemediations() {
			if !r.Status.Stage.Terminal() {
				pending = append(pending, r.Name+"@"+string(r.Status.Stage))
				continue
			}
			if r.Status.Outcome != outcome {
				gomega.StopTrying(fmt.Sprintf("remediation %s finished with outcome %q, want %q; trace %v; message %q",
					r.Name, r.Status.Outcome, outcome, StageTraceOf(&r), r.Status.Message)).Now()
			}
			done = r.DeepCopy()
			return
		}
		g.Expect(done).NotTo(gomega.BeNil(), "no new remediation finished yet; in flight: %v", pending)
	}).WithTimeout(RemediationTimeout).WithPolling(DefaultPoll).Should(gomega.Succeed())
	return done
}

// WaitNewRemediation waits until a remediation created after New exists
// (any stage) and returns it.
func (f *F) WaitNewRemediation() *v1alpha1.SmbMountRemediation {
	f.T.Helper()
	var r *v1alpha1.SmbMountRemediation
	f.G.Eventually(func(g gomega.Gomega) {
		rs := f.NewRemediations()
		g.Expect(rs).NotTo(gomega.BeEmpty())
		r = rs[0].DeepCopy()
	}).WithTimeout(RemediationTimeout).Should(gomega.Succeed())
	return r
}

// StageTrace returns the stages the named remediation went through, from
// status.transitions (bounded to the last v1alpha1.MaxTransitions).
func (f *F) StageTrace(name string) []v1alpha1.Stage {
	f.T.Helper()
	var r v1alpha1.SmbMountRemediation
	f.G.Expect(f.Client.Get(f.Ctx, keyOf(name), &r)).To(gomega.Succeed())
	return StageTraceOf(&r)
}

// StageTraceOf is StageTrace for an object already in hand: the first
// transition's From (if set) followed by every To.
func StageTraceOf(r *v1alpha1.SmbMountRemediation) []v1alpha1.Stage {
	var out []v1alpha1.Stage
	for i, tr := range r.Status.Transitions {
		if i == 0 && tr.From != "" {
			out = append(out, tr.From)
		}
		out = append(out, tr.To)
	}
	return out
}

// FormatTrace renders a stage trace as "a -> b -> c".
func FormatTrace(tr []v1alpha1.Stage) string {
	s := make([]string, len(tr))
	for i, st := range tr {
		s[i] = string(st)
	}
	return strings.Join(s, " -> ")
}

// keyOf is the ObjectKey of a cluster-scoped object.
func keyOf(name string) client.ObjectKey { return client.ObjectKey{Name: name} }

func ownedBy(refs []metav1.OwnerReference, name string) bool {
	for _, r := range refs {
		if r.Kind == "SmbMountRemediation" && r.Name == name {
			return true
		}
	}
	return false
}
