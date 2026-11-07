package remediation

import (
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

// mirror emits PatchSmbMountRemediationMirror for every affected SmbMount
// whose control-plane fields differ from the desired ones (level-triggered:
// nothing is emitted once the SmbMounts show the mirror).
//
// The patch is a server-side apply by the control-plane field manager, so it
// carries through the fields of that manager it does not own here
// (remediationsLastHour, the RemediationSuppressed condition) and, while
// active, the existing lastRemediation.
func (m *machine) mirror() {
	names := map[string]bool{}
	for _, n := range m.obj.Spec.Trigger.SmbMounts {
		names[n] = true
	}
	for _, pv := range m.scopePVs() {
		names[v1alpha1.SmbMountName(m.node, pv)] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		sm := m.idx.mountByName[n]
		if sm == nil {
			continue
		}
		want, ok := m.desiredMirror(sm)
		if !ok || mirrorMatches(sm.Status, want) {
			continue
		}
		m.emit(want)
	}
}

func (m *machine) desiredMirror(sm *v1alpha1.SmbMount) (command.PatchSmbMountRemediationMirror, bool) {
	cur := sm.Status
	p := command.PatchSmbMountRemediationMirror{SmbMount: sm.Name}
	if cur.RemediationsLastHour > 0 {
		v := cur.RemediationsLastHour
		p.RemediationsLastHour = &v
	}
	var conds []metav1.Condition
	keep := func(t string) {
		if c := findCondition(cur.Conditions, t); c != nil {
			conds = append(conds, *c)
		}
	}
	keep(v1alpha1.ConditionRemediationSuppressed)

	if !m.st.Stage.Terminal() {
		p.Remediation = &v1alpha1.ActiveRemediation{
			Active: true, Name: m.obj.Name, Stage: m.st.Stage, StageEnteredTime: m.st.StageEnteredTime,
		}
		if cur.LastRemediation != nil {
			lr := *cur.LastRemediation
			p.LastRemediation = &lr
		}
		conds = upsert(conds, cur.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionRemediating, Status: metav1.ConditionTrue,
			Reason: string(m.st.Stage), Message: "remediation " + m.obj.Name + " in stage " + string(m.st.Stage),
		}, m.nowS)
		keep(v1alpha1.ConditionDegraded)
	} else {
		// Never clobber a newer remediation's mirror.
		if r := cur.Remediation; r != nil && r.Active && r.Name != m.obj.Name {
			return p, false
		}
		if lr := cur.LastRemediation; lr != nil && lr.Name != m.obj.Name && lr.StartedTime != nil &&
			m.st.StartedTime != nil && lr.StartedTime.After(m.st.StartedTime.Time) {
			return p, false
		}
		p.LastRemediation = &v1alpha1.LastRemediation{
			Name: m.obj.Name, Outcome: m.st.Outcome, StartedTime: m.st.StartedTime, FinishedTime: m.st.StageEnteredTime,
		}
		conds = upsert(conds, cur.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionRemediating, Status: metav1.ConditionFalse,
			Reason: "Idle", Message: "no active remediation",
		}, m.nowS)
		deg := metav1.ConditionFalse
		switch m.st.Outcome {
		case v1alpha1.OutcomeHealed, v1alpha1.OutcomeDryRun, "":
		case v1alpha1.OutcomeVerifyFailed, v1alpha1.OutcomeSkippedNoOwner, v1alpha1.OutcomeRolledBack, v1alpha1.OutcomeFailed:
			deg = metav1.ConditionTrue
		}
		reason := string(m.st.Outcome)
		if reason == "" {
			reason = "Unknown"
		}
		conds = upsert(conds, cur.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionDegraded, Status: deg, Reason: reason,
			Message: "last remediation " + m.obj.Name + ": " + reason,
		}, m.nowS)
	}
	sort.Slice(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })
	p.Conditions = conds
	return p, true
}

// upsert appends c, keeping the observed LastTransitionTime when the status is unchanged.
func upsert(conds, observed []metav1.Condition, c metav1.Condition, now metav1.Time) []metav1.Condition {
	c.LastTransitionTime = now
	if o := findCondition(observed, c.Type); o != nil && o.Status == c.Status {
		c.LastTransitionTime = o.LastTransitionTime
	}
	return append(conds, c)
}

func mirrorMatches(cur v1alpha1.SmbMountStatus, p command.PatchSmbMountRemediationMirror) bool {
	if !activeEqual(cur.Remediation, p.Remediation) || !lastEqual(cur.LastRemediation, p.LastRemediation) {
		return false
	}
	for _, t := range []string{v1alpha1.ConditionRemediating, v1alpha1.ConditionDegraded} {
		want := findCondition(p.Conditions, t)
		got := findCondition(cur.Conditions, t)
		if (want == nil) != (got == nil) {
			return false
		}
		if want != nil && (want.Status != got.Status || want.Reason != got.Reason || want.Message != got.Message) {
			return false
		}
	}
	return true
}

func timeEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b)
}

func activeEqual(a, b *v1alpha1.ActiveRemediation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Active == b.Active && a.Name == b.Name && a.Stage == b.Stage && timeEqual(a.StageEnteredTime, b.StageEnteredTime)
}

func lastEqual(a, b *v1alpha1.LastRemediation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Name == b.Name && a.Outcome == b.Outcome && timeEqual(a.StartedTime, b.StartedTime) &&
		timeEqual(a.FinishedTime, b.FinishedTime)
}
