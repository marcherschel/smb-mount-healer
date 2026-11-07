package controlplane

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

// SmbMountStatusWriter is the single path for every control-plane write of
// SmbMount status.
//
// All control-plane status applies share v1alpha1.FieldManagerControlPlane,
// and a server-side apply that omits a field this manager owned deletes it.
// The control plane writes two field groups from two controllers:
//
//   - mirror (RemediationReconciler, via remediation.Step's
//     PatchSmbMountRemediationMirror): remediation, lastRemediation and the
//     Remediating and Degraded conditions;
//   - trigger (TriggerController): remediationsLastHour and the
//     RemediationSuppressed condition.
//
// Each write replaces only its own group: under one mutex the writer reads
// the SmbMount live (uncached, so a write that is not yet in the informer
// cache is never lost), takes the other group's values from it, and applies
// the full control-plane field set. Nothing is sent when the result equals
// the live values. Conditions whose status is unchanged keep their live
// lastTransitionTime.
type SmbMountStatusWriter struct {
	reader client.Reader
	exec   command.ClusterExecutor
	clock  clock.PassiveClock
	mu     sync.Mutex
}

// NewSmbMountStatusWriter returns a writer that reads SmbMounts through
// reader (use an uncached reader, e.g. mgr.GetAPIReader()) and applies
// through exec (the control plane's Executor).
func NewSmbMountStatusWriter(reader client.Reader, exec command.ClusterExecutor, clk clock.PassiveClock) *SmbMountStatusWriter {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &SmbMountStatusWriter{reader: reader, exec: exec, clock: clk}
}

// cpFields is the complete set of SmbMount status fields the control plane owns.
type cpFields struct {
	Remediation          *v1alpha1.ActiveRemediation
	LastRemediation      *v1alpha1.LastRemediation
	RemediationsLastHour int32
	Remediating          *metav1.Condition
	Degraded             *metav1.Condition
	Suppressed           *metav1.Condition
}

func currentCPFields(st *v1alpha1.SmbMountStatus) cpFields {
	cond := func(t string) *metav1.Condition {
		if c := meta.FindStatusCondition(st.Conditions, t); c != nil {
			cc := *c
			return &cc
		}
		return nil
	}
	f := cpFields{
		RemediationsLastHour: st.RemediationsLastHour,
		Remediating:          cond(v1alpha1.ConditionRemediating),
		Degraded:             cond(v1alpha1.ConditionDegraded),
		Suppressed:           cond(v1alpha1.ConditionRemediationSuppressed),
	}
	if st.Remediation != nil {
		r := *st.Remediation
		f.Remediation = &r
	}
	if st.LastRemediation != nil {
		l := *st.LastRemediation
		f.LastRemediation = &l
	}
	return f
}

// TriggerFields is the trigger group of SmbMount status.
type TriggerFields struct {
	RemediationsLastHour int32
	// Suppressed is the RemediationSuppressed condition; nil omits it
	// (releases it, which removes it from the object).
	Suppressed *metav1.Condition
}

// triggerFieldsMatch reports whether st already shows want (ignoring
// lastTransitionTime of an unchanged condition).
func triggerFieldsMatch(st *v1alpha1.SmbMountStatus, want TriggerFields) bool {
	return st.RemediationsLastHour == want.RemediationsLastHour &&
		condMatches(meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionRemediationSuppressed), want.Suppressed)
}

func condMatches(got, want *metav1.Condition) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Status == want.Status && got.Reason == want.Reason && got.Message == want.Message
}

// ApplyMirror writes the mirror group of cmd (Remediation, LastRemediation,
// the Remediating and Degraded conditions) and carries the trigger group
// through from the live object. A missing SmbMount is not an error.
func (w *SmbMountStatusWriter) ApplyMirror(ctx context.Context, cmd command.PatchSmbMountRemediationMirror) error {
	return w.update(ctx, cmd.SmbMount, func(f *cpFields) {
		f.Remediation = cmd.Remediation
		f.LastRemediation = cmd.LastRemediation
		f.Remediating = findCond(cmd.Conditions, v1alpha1.ConditionRemediating)
		f.Degraded = findCond(cmd.Conditions, v1alpha1.ConditionDegraded)
	})
}

// ApplyTrigger writes the trigger group and carries the mirror group
// through from the live object. A missing SmbMount is not an error.
// newlySuppressed reports whether this write turned RemediationSuppressed
// True or changed its reason, judged against the live object, so a caller
// working from a lagging cache never counts one suppression twice.
func (w *SmbMountStatusWriter) ApplyTrigger(ctx context.Context, name string, tf TriggerFields) (newlySuppressed bool, err error) {
	err = w.update(ctx, name, func(f *cpFields) {
		prev := f.Suppressed
		f.RemediationsLastHour = tf.RemediationsLastHour
		f.Suppressed = nil
		if tf.Suppressed != nil {
			c := *tf.Suppressed
			f.Suppressed = &c
			newlySuppressed = c.Status == metav1.ConditionTrue &&
				(prev == nil || prev.Status != metav1.ConditionTrue || prev.Reason != c.Reason)
		}
	})
	if err != nil {
		return false, err
	}
	return newlySuppressed, nil
}

func findCond(conds []metav1.Condition, t string) *metav1.Condition {
	if c := meta.FindStatusCondition(conds, t); c != nil {
		cc := *c
		return &cc
	}
	return nil
}

func (w *SmbMountStatusWriter) update(ctx context.Context, name string, mutate func(*cpFields)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var sm v1alpha1.SmbMount
	if err := w.reader.Get(ctx, client.ObjectKey{Name: name}, &sm); err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get SmbMount %s: %w", name, err)
	}
	cur := currentCPFields(&sm.Status)
	next := currentCPFields(&sm.Status)
	mutate(&next)
	now := metav1.NewTime(w.clock.Now().Truncate(time.Second))
	for _, p := range []struct{ next, cur *metav1.Condition }{
		{next.Remediating, cur.Remediating}, {next.Degraded, cur.Degraded}, {next.Suppressed, cur.Suppressed},
	} {
		if p.next == nil {
			continue
		}
		switch {
		case p.cur != nil && p.cur.Status == p.next.Status && !p.cur.LastTransitionTime.IsZero():
			p.next.LastTransitionTime = p.cur.LastTransitionTime
		case p.next.LastTransitionTime.IsZero():
			p.next.LastTransitionTime = now
		}
	}
	if equality.Semantic.DeepEqual(cur, next) {
		return nil
	}
	return w.exec.ExecuteCluster(ctx, next.command(name))
}

func (f cpFields) command(name string) command.PatchSmbMountRemediationMirror {
	c := command.PatchSmbMountRemediationMirror{SmbMount: name, Remediation: f.Remediation, LastRemediation: f.LastRemediation}
	if f.RemediationsLastHour > 0 {
		v := f.RemediationsLastHour
		c.RemediationsLastHour = &v
	}
	for _, cond := range []*metav1.Condition{f.Remediating, f.Degraded, f.Suppressed} {
		if cond != nil {
			c.Conditions = append(c.Conditions, *cond)
		}
	}
	sort.Slice(c.Conditions, func(i, j int) bool { return c.Conditions[i].Type < c.Conditions[j].Type })
	return c
}
