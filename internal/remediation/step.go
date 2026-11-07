package remediation

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

// maxHops bounds the number of transitions a single Step may chain (every
// guard that passes immediately, e.g. in dry run, is followed in the same
// Step). The longest real chain is well below this.
const maxHops = 32

// Step advances one remediation. It must not mutate obj.
//
// Step is level-triggered: wait-state commands are a function of (status,
// observation) and are re-emitted on every call until their guard flips, so
// Step(Step(obj, obs).Status, obs) emits the same wait-state commands with an
// unchanged status. Transition-only side effects are Events and, for a few
// non-waited host tasks (ProbeNow), the CreateHostTask of the step that
// leaves the stage.
//
// The caller must persist Result.Status before executing Result.Commands
// (write-ahead). Every timestamp written is truncated to seconds, so the
// persisted status round-trips through the API server unchanged.
func Step(obj *v1alpha1.SmbMountRemediation, obs Observation, cfg Config) Result {
	m := newMachine(obj, obs, cfg)

	if m.st.Stage == "" {
		m.st.StartedTime = m.nowPtr()
		if m.typ == v1alpha1.RemediationTypeSweep {
			m.move(v1alpha1.StagePlanning, ReasonCreated)
		} else {
			m.move(v1alpha1.StagePending, ReasonCreated)
		}
	}
	if m.st.StartedTime == nil {
		m.st.StartedTime = m.nowPtr()
	}

	for hop := 0; hop < maxHops && !m.st.Stage.Terminal(); hop++ {
		before, n := m.st.Stage, len(m.trans)
		if !m.globalGuards() {
			m.stepStage()
		}
		if m.st.Stage == before && len(m.trans) == n {
			break // waiting
		}
	}

	m.refreshHostTaskPhases()
	m.mirror()

	return Result{
		Status:       *m.st,
		Commands:     dedupe(m.cmds),
		Transitions:  m.trans,
		RequeueAfter: m.requeue(),
	}
}

type machine struct {
	obj   *v1alpha1.SmbMountRemediation
	obs   Observation
	cfg   Config
	st    *v1alpha1.SmbMountRemediationStatus
	typ   v1alpha1.RemediationType
	node  string
	dry   bool
	now   time.Time   // obs.Now, full precision, for comparisons
	nowS  metav1.Time // obs.Now truncated to seconds, for everything written
	idx   index
	cmds  []command.ClusterCommand
	trans []Transition
	// outcome carries the outcome of the transition into finalizing.
	outcome v1alpha1.Outcome
	message string
}

func newMachine(obj *v1alpha1.SmbMountRemediation, obs Observation, cfg Config) *machine {
	typ := obj.Spec.Type
	if typ == "" {
		typ = v1alpha1.RemediationTypeWorkload
	}
	return &machine{
		obj:  obj,
		obs:  obs,
		cfg:  cfg,
		st:   obj.Status.DeepCopy(),
		typ:  typ,
		node: obj.Spec.NodeName,
		dry:  obj.Spec.DryRun || cfg.DryRun,
		now:  obs.Now,
		nowS: metav1.NewTime(obs.Now.Truncate(time.Second)),
		idx:  buildIndex(obj, obs),
	}
}

func (m *machine) nowPtr() *metav1.Time { t := m.nowS; return &t }

func (m *machine) emit(c command.ClusterCommand) { m.cmds = append(m.cmds, c) }

func (m *machine) self() command.ObjectRef {
	return command.ObjectRef{Kind: "SmbMountRemediation", Name: m.obj.Name, UID: m.obj.UID}
}

func (m *machine) event(typ, reason, action, msg string) {
	m.emit(command.EmitEvent{Regarding: m.self(), Type: typ, Reason: reason, Action: action, Message: msg})
}

// move records a transition and runs the entry actions of the new stage.
func (m *machine) move(to v1alpha1.Stage, reason string) {
	from := m.st.Stage
	m.trans = append(m.trans, Transition{From: from, To: to, Reason: reason, Time: m.nowS.Time})
	m.st.Transitions = append(m.st.Transitions, v1alpha1.StageTransition{From: from, To: to, Reason: reason, Time: m.nowS})
	if n := len(m.st.Transitions); n > v1alpha1.MaxTransitions {
		m.st.Transitions = append([]v1alpha1.StageTransition(nil), m.st.Transitions[n-v1alpha1.MaxTransitions:]...)
	}
	m.st.Stage = to
	m.st.StageEnteredTime = m.nowPtr()
	m.st.Deadline = nil
	m.enter(to)
}

func (m *machine) setDeadline(d time.Duration) {
	t := metav1.NewTime(m.nowS.Add(d))
	m.st.Deadline = &t
}

func (m *machine) deadlinePassed() bool {
	return m.st.Deadline != nil && !m.now.Before(m.st.Deadline.Time)
}

// enter runs the entry actions of a stage. Everything computed here is
// persisted in status, so a resumed Step never recomputes it.
func (m *machine) enter(s v1alpha1.Stage) {
	switch s {
	case v1alpha1.StagePending:
	case v1alpha1.StagePlanning:
		m.setDeadline(m.cfg.PlanningProbeWait)
		if m.typ == v1alpha1.RemediationTypeWorkload {
			// First sight: record scope (and originalReplicas from the live
			// spec) immediately so other remediations see our claim.
			m.st.Scope = m.mergeScope(m.planWorkloadScope())
		}
	case v1alpha1.StageDraining:
		m.setDeadline(m.cfg.ScaleDownWait)
	case v1alpha1.StageReleasingMounts:
	case v1alpha1.StageAwaitingUnstage:
		m.setDeadline(m.cfg.UnstageWait)
		m.st.DeadGlobalmounts = m.deadGlobalmounts(m.scopePVs(), false)
	case v1alpha1.StageQuarantining:
	case v1alpha1.StageAwaitingUnstageCompletion:
		m.setDeadline(m.cfg.QuarantineUnstageWait)
	case v1alpha1.StageRestartingDriver:
		m.setDeadline(m.cfg.DriverRestartWait)
	case v1alpha1.StageScalingUp:
	case v1alpha1.StageVerifying:
		m.setDeadline(m.cfg.VerifyWait)
		m.st.Verification = nil
		for _, w := range m.scopeWorkloads() {
			m.st.Verification = append(m.st.Verification, v1alpha1.WorkloadVerification{
				Workload: w.WorkloadRef, Result: v1alpha1.VerificationPending,
			})
		}
	case v1alpha1.StageSweeping:
	case v1alpha1.StageRollingBack:
		m.setDeadline(m.cfg.ScaleDownWait)
	case v1alpha1.StageFinalizing:
		out := m.outcome
		if out == "" {
			out = m.defaultOutcome()
		}
		if m.dry && out != v1alpha1.OutcomeRolledBack && out != v1alpha1.OutcomeSkippedNoOwner {
			out = v1alpha1.OutcomeDryRun
		}
		m.st.Outcome = out
		if m.message != "" {
			m.st.Message = m.message
		} else {
			m.st.Message = m.defaultMessage(out)
		}
	case v1alpha1.StageDone:
	}
}

func (m *machine) finalize(reason string, out v1alpha1.Outcome, msg string) {
	m.outcome, m.message = out, msg
	m.move(v1alpha1.StageFinalizing, reason)
}

func (m *machine) defaultOutcome() v1alpha1.Outcome {
	for _, v := range m.st.Verification {
		if v.Result == v1alpha1.VerificationFailed {
			return v1alpha1.OutcomeVerifyFailed
		}
	}
	return v1alpha1.OutcomeHealed
}

func (m *machine) defaultMessage(out v1alpha1.Outcome) string {
	switch out {
	case v1alpha1.OutcomeHealed:
		return fmt.Sprintf("remediation finished: %d workload(s), %d PV(s)", len(m.scopeWorkloads()), len(m.scopePVs()))
	case v1alpha1.OutcomeVerifyFailed:
		var failed []string
		for _, v := range m.st.Verification {
			if v.Result == v1alpha1.VerificationFailed {
				failed = append(failed, v.Workload.String())
			}
		}
		return "verification failed for " + strings.Join(failed, ", ")
	case v1alpha1.OutcomeSkippedNoOwner:
		return "no Deployment/StatefulSet owns the broken pods; manual restart needed"
	case v1alpha1.OutcomeRolledBack:
		return "rolled back"
	case v1alpha1.OutcomeDryRun:
		return "dry run: no workload, pod or host was changed"
	case v1alpha1.OutcomeFailed:
		return "remediation failed"
	}
	return ""
}

// globalGuards sends a non-terminal remediation to rolling_back. It
// reports whether it transitioned.
func (m *machine) globalGuards() bool {
	switch m.st.Stage {
	case v1alpha1.StageRollingBack, v1alpha1.StageFinalizing, v1alpha1.StageDone:
		return false
	case v1alpha1.StagePending, v1alpha1.StagePlanning, v1alpha1.StageDraining,
		v1alpha1.StageReleasingMounts, v1alpha1.StageAwaitingUnstage, v1alpha1.StageQuarantining,
		v1alpha1.StageAwaitingUnstageCompletion, v1alpha1.StageRestartingDriver,
		v1alpha1.StageScalingUp, v1alpha1.StageVerifying, v1alpha1.StageSweeping:
	}
	reason := ""
	switch {
	case m.obs.Deleting:
		reason = ReasonDeleted
	case m.obs.Aborted:
		reason = ReasonAborted
		setCondition(&m.st.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionAborted, Status: metav1.ConditionTrue, Reason: "AbortAnnotation",
			Message: "annotation " + v1alpha1.AnnotationAbort + "=true",
		}, m.nowS)
	case m.cfg.EpisodeMaxDuration > 0 && m.st.StartedTime != nil &&
		!m.now.Before(m.st.StartedTime.Add(m.cfg.EpisodeMaxDuration)):
		reason = ReasonEpisodeTimeout
	case m.typ == v1alpha1.RemediationTypeWorkload && m.allWorkloadsDeleted():
		reason = ReasonWorkloadsDeleted
	}
	if reason == "" {
		return false
	}
	m.event(command.EventWarning, "RollingBack", "Rollback",
		fmt.Sprintf("rolling back from stage %s: %s", m.st.Stage, reason))
	m.move(v1alpha1.StageRollingBack, reason)
	return true
}

func (m *machine) allWorkloadsDeleted() bool {
	switch m.st.Stage {
	case v1alpha1.StageDraining, v1alpha1.StageReleasingMounts, v1alpha1.StageAwaitingUnstage,
		v1alpha1.StageQuarantining, v1alpha1.StageAwaitingUnstageCompletion,
		v1alpha1.StageRestartingDriver, v1alpha1.StageScalingUp, v1alpha1.StageVerifying:
	case v1alpha1.StagePending, v1alpha1.StagePlanning, v1alpha1.StageSweeping,
		v1alpha1.StageRollingBack, v1alpha1.StageFinalizing, v1alpha1.StageDone:
		return false
	}
	ws := m.scopeWorkloads()
	if len(ws) == 0 {
		return false
	}
	for _, w := range ws {
		o, ok := m.idx.workload(w.WorkloadRef)
		if !ok || o.Exists {
			return false
		}
	}
	return true
}

// stepStage evaluates the current stage once.
func (m *machine) stepStage() {
	if m.typ == v1alpha1.RemediationTypeSweep {
		m.stepSweep()
		return
	}
	m.stepWorkload()
}

func (m *machine) stepCommon(s v1alpha1.Stage) {
	switch s {
	case v1alpha1.StageRollingBack:
		m.stepRollingBack()
	case v1alpha1.StageFinalizing:
		m.stepFinalizing()
	case v1alpha1.StageAwaitingUnstageCompletion:
		m.stepAwaitingUnstageCompletion()
	case v1alpha1.StagePending, v1alpha1.StagePlanning, v1alpha1.StageDraining,
		v1alpha1.StageReleasingMounts, v1alpha1.StageAwaitingUnstage, v1alpha1.StageQuarantining,
		v1alpha1.StageRestartingDriver, v1alpha1.StageScalingUp, v1alpha1.StageVerifying,
		v1alpha1.StageSweeping, v1alpha1.StageDone:
	}
}

func (m *machine) stepFinalizing() {
	out := m.st.Outcome
	typ := command.EventNormal
	switch out {
	case v1alpha1.OutcomeHealed, v1alpha1.OutcomeDryRun:
	case v1alpha1.OutcomeVerifyFailed, v1alpha1.OutcomeSkippedNoOwner, v1alpha1.OutcomeRolledBack, v1alpha1.OutcomeFailed:
		typ = command.EventWarning
	}
	m.event(typ, "RemediationFinished", "Finalize", fmt.Sprintf("outcome %s: %s", out, m.st.Message))
	m.move(v1alpha1.StageDone, ReasonFinalized)
}

// stepRollingBack restores originalReplicas of every workload not yet restored.
func (m *machine) stepRollingBack() {
	if m.dry {
		m.finalize(ReasonDryRun, v1alpha1.OutcomeRolledBack, "")
		return
	}
	var pending []string
	for i := range m.scopeWorkloadsMut() {
		w := &m.st.Scope.Workloads[i]
		switch w.State {
		case v1alpha1.WorkloadRestored, v1alpha1.WorkloadDeleted:
			continue
		case v1alpha1.WorkloadInScope, v1alpha1.WorkloadScaledDown, "":
		}
		o, ok := m.idx.workload(w.WorkloadRef)
		switch {
		case ok && !o.Exists:
			w.State = v1alpha1.WorkloadDeleted
		case ok && o.Replicas == w.OriginalReplicas:
			w.State = v1alpha1.WorkloadRestored
		default:
			m.emit(command.ScaleWorkload{Workload: w.WorkloadRef, Replicas: w.OriginalReplicas, Action: command.ActionRollback})
			pending = append(pending, w.String())
		}
	}
	if len(pending) == 0 {
		m.finalize(ReasonRestored, v1alpha1.OutcomeRolledBack, "")
		return
	}
	if m.deadlinePassed() {
		msg := "replicas not restored within the wait for: " + strings.Join(pending, ", ")
		m.event(command.EventWarning, "RestoreTimeout", "Rollback", msg)
		m.finalize(ReasonRestoreTimeout, v1alpha1.OutcomeRolledBack, "rolled back; "+msg)
	}
}

// stepAwaitingUnstageCompletion waits until kubelet removed every scoped
// globalmount directory (or it is mounted again).
func (m *machine) stepAwaitingUnstageCompletion() {
	next := v1alpha1.StageRestartingDriver
	if m.typ == v1alpha1.RemediationTypeSweep {
		next = v1alpha1.StageFinalizing
	} else {
		m.holdScaledDown()
	}
	var pending []string
	for _, pv := range m.scopePVs() {
		sm := m.idx.mountByPV[pv]
		if sm == nil || sm.Status.Globalmount == nil {
			continue
		}
		g := sm.Status.Globalmount
		if !g.DirExists || g.Mounted {
			continue
		}
		// A dead orphan globalmount the Sweep force-unmounted may never have
		// been staged by kubelet (e.g. mounted by hand): its empty dir is
		// never removed, and it does not block a later NodeStageVolume, so
		// there is no unstage to wait for (the bash healer did not wait
		// either). Every other dir is waited for: a dir that held shadow data
		// blocked kubelet's unstage, and only kubelet removing it re-arms a
		// fresh NodeStageVolume.
		if m.typ == v1alpha1.RemediationTypeSweep && g.Empty && m.sweptDeadGlobalmount(pv) {
			continue
		}
		pending = append(pending, pv)
	}
	goTo := func(reason string) {
		if next == v1alpha1.StageFinalizing {
			m.finalize(reason, v1alpha1.OutcomeHealed, "")
			return
		}
		m.move(next, reason)
	}
	switch {
	case len(pending) == 0:
		goTo(ReasonUnstageCompleted)
	case m.dry:
		goTo(ReasonDryRun)
	case m.deadlinePassed():
		var pins []string
		for _, pv := range pending {
			for _, p := range m.idx.podsUsingPV(pv) {
				pins = append(pins, fmt.Sprintf("%s/%s (pv=%s)", p.Namespace, p.Name, pv))
			}
		}
		msg := "globalmount dir(s) still present for pv " + strings.Join(pending, ", ")
		if len(pins) > 0 {
			msg += "; kubelet will not unstage while these pods exist: " + strings.Join(pins, ", ")
		}
		m.event(command.EventWarning, "UnstagePinned", "AwaitUnstage", msg)
		goTo(ReasonUnstageTimeout)
	}
}

// sweptDeadGlobalmount reports whether pv's globalmount was a dead mount the
// Sweep force-unmounted.
func (m *machine) sweptDeadGlobalmount(pv string) bool {
	for _, d := range m.st.DeadGlobalmounts {
		if d.PVName == pv {
			return true
		}
	}
	return false
}

// requeue computes RequeueAfter: 0 when terminal, else
// min(deadline-now, episode end-now, PollInterval).
func (m *machine) requeue() time.Duration {
	if m.st.Stage.Terminal() {
		return 0
	}
	r := m.cfg.PollInterval
	if r <= 0 {
		r = time.Second
	}
	if m.st.Deadline != nil {
		if d := m.st.Deadline.Sub(m.now); d > 0 && d < r {
			r = d
		}
	}
	if m.cfg.EpisodeMaxDuration > 0 && m.st.StartedTime != nil {
		if d := m.st.StartedTime.Add(m.cfg.EpisodeMaxDuration).Sub(m.now); d > 0 && d < r {
			r = d
		}
	}
	return r
}

// dedupe drops later commands with a Key already emitted, keeping order.
func dedupe(in []command.ClusterCommand) []command.ClusterCommand {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]command.ClusterCommand, 0, len(in))
	for _, c := range in {
		k := fmt.Sprintf("%T/%s", c, c.Key())
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	return out
}

// setCondition upserts a condition, keeping LastTransitionTime when the
// status did not change. Conditions stay sorted by type.
func setCondition(conds *[]metav1.Condition, c metav1.Condition, now metav1.Time) {
	for i := range *conds {
		cur := &(*conds)[i]
		if cur.Type != c.Type {
			continue
		}
		if cur.Status == c.Status {
			c.LastTransitionTime = cur.LastTransitionTime
		} else {
			c.LastTransitionTime = now
		}
		*cur = c
		return
	}
	c.LastTransitionTime = now
	*conds = append(*conds, c)
	sort.Slice(*conds, func(i, j int) bool { return (*conds)[i].Type < (*conds)[j].Type })
}

func findCondition(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}
