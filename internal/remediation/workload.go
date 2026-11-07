package remediation

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

// CRD list bounds (see api/v1alpha1).
const (
	maxScopeWorkloads = 128
	maxScopePVs       = 128
	maxDrainPods      = 512
	maxHostActions    = 256
	maxHostTaskRefs   = 64
)

func (m *machine) stepWorkload() {
	switch s := m.st.Stage; s {
	case v1alpha1.StagePending:
		m.stepPending()
	case v1alpha1.StagePlanning:
		m.stepPlanningWorkload()
	case v1alpha1.StageDraining:
		m.stepDraining()
	case v1alpha1.StageReleasingMounts:
		m.stepReleasingMounts()
	case v1alpha1.StageAwaitingUnstage:
		m.stepAwaitingUnstage()
	case v1alpha1.StageQuarantining:
		m.stepQuarantining()
	case v1alpha1.StageRestartingDriver:
		m.stepRestartingDriver()
	case v1alpha1.StageScalingUp:
		m.stepScalingUp()
	case v1alpha1.StageVerifying:
		m.stepVerifying()
	case v1alpha1.StageAwaitingUnstageCompletion, v1alpha1.StageRollingBack,
		v1alpha1.StageFinalizing, v1alpha1.StageDone:
		m.stepCommon(s)
	case v1alpha1.StageSweeping:
		// Not a Workload stage; roll back defensively.
		m.move(v1alpha1.StageRollingBack, ReasonAborted)
	}
}

// ---------------- scope helpers ----------------

func (m *machine) scopeWorkloads() []v1alpha1.ScopedWorkload {
	if m.st.Scope == nil {
		return nil
	}
	return m.st.Scope.Workloads
}

// scopeWorkloadsMut returns the scope workloads for in-place updates.
func (m *machine) scopeWorkloadsMut() []v1alpha1.ScopedWorkload { return m.scopeWorkloads() }

func (m *machine) scopePVs() []string {
	if m.st.Scope == nil {
		return nil
	}
	return m.st.Scope.PVs
}

func (m *machine) drainUIDs() map[types.UID]bool {
	out := map[types.UID]bool{}
	if m.st.Scope != nil {
		for _, p := range m.st.Scope.DrainPods {
			out[p.UID] = true
		}
	}
	return out
}

func (m *machine) inScope(ref v1alpha1.WorkloadRef) bool {
	for _, w := range m.scopeWorkloads() {
		if w.WorkloadRef == ref {
			return true
		}
	}
	return false
}

// plannedScope is the result of planning, before replica bookkeeping.
type plannedScope struct {
	owners    map[string]v1alpha1.WorkloadRef
	pvs       map[string]bool
	drain     map[types.UID]v1alpha1.PodRef
	unowned   []string // ns/name of unowned broken pods or co-tenants (informational)
	cotenants map[string]bool
}

// planWorkloadScope computes the scope from the observation: owners of the
// broken pods of the triggering SmbMounts, all their smb PVs (siblings), and
// every pod on the node using one of those PVs whose globalmount is not
// healthy (co-tenants; one level, like the bash healer).
func (m *machine) planWorkloadScope() plannedScope {
	ps := plannedScope{
		owners: map[string]v1alpha1.WorkloadRef{}, pvs: map[string]bool{},
		drain: map[types.UID]v1alpha1.PodRef{}, cotenants: map[string]bool{},
	}
	addPod := func(p *PodObservation) {
		ps.owners[p.Owner.String()] = *p.Owner
		ps.drain[p.UID] = v1alpha1.PodRef{Namespace: p.Namespace, Name: p.Name, UID: p.UID}
		for _, pv := range p.PVs {
			ps.pvs[pv] = true
		}
	}
	for _, name := range sortedUnique(m.obj.Spec.Trigger.SmbMounts) {
		sm := m.idx.mountByName[name]
		if sm == nil {
			continue
		}
		for _, pm := range sm.Status.PodMounts {
			if pm.Health != v1alpha1.HealthBroken {
				continue
			}
			p := m.idx.podByUID[pm.UID]
			if p == nil || p.Terminating {
				continue
			}
			if p.Owner == nil {
				ps.unowned = append(ps.unowned, p.Namespace+"/"+p.Name)
				continue
			}
			addPod(p)
		}
	}
	for pv := range ps.pvs {
		if !m.idx.globalmountHealthy(pv) {
			ps.cotenants[pv] = true
		}
	}
	for i := range m.idx.pods {
		p := &m.idx.pods[i]
		if _, ok := ps.drain[p.UID]; ok || p.Terminating {
			continue
		}
		uses := false
		for _, pv := range p.PVs {
			if ps.cotenants[pv] {
				uses = true
			}
		}
		if !uses {
			continue
		}
		if p.Owner == nil {
			ps.unowned = append(ps.unowned, p.Namespace+"/"+p.Name)
			continue
		}
		addPod(p)
	}
	// Every other pod of a scoped workload on the node must drain too.
	for i := range m.idx.pods {
		p := &m.idx.pods[i]
		if p.Owner == nil {
			continue
		}
		if _, ok := ps.owners[p.Owner.String()]; ok {
			ps.drain[p.UID] = v1alpha1.PodRef{Namespace: p.Namespace, Name: p.Name, UID: p.UID}
		}
	}
	sort.Strings(ps.unowned)
	return ps
}

// mergeScope turns a plan into a status scope. originalReplicas of
// workloads already in status are kept (status is the source of truth);
// new workloads record the live spec.replicas. Workloads that do not exist
// (or are not observed) are dropped.
func (m *machine) mergeScope(ps plannedScope) *v1alpha1.RemediationScope {
	prev := map[string]v1alpha1.ScopedWorkload{}
	for _, w := range m.scopeWorkloads() {
		prev[w.String()] = w
	}
	keys := make([]string, 0, len(ps.owners))
	for k := range ps.owners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sc := &v1alpha1.RemediationScope{}
	keep := map[string]bool{}
	for _, k := range keys {
		if len(sc.Workloads) >= maxScopeWorkloads {
			break
		}
		if w, ok := prev[k]; ok {
			sc.Workloads = append(sc.Workloads, w)
			keep[k] = true
			continue
		}
		o, ok := m.idx.workload(ps.owners[k])
		if !ok || !o.Exists {
			continue
		}
		sc.Workloads = append(sc.Workloads, v1alpha1.ScopedWorkload{
			WorkloadRef: ps.owners[k], OriginalReplicas: o.Replicas, State: v1alpha1.WorkloadInScope,
		})
		keep[k] = true
	}
	for pv := range ps.pvs {
		sc.PVs = append(sc.PVs, pv)
	}
	sort.Strings(sc.PVs)
	if len(sc.PVs) > maxScopePVs {
		sc.PVs = sc.PVs[:maxScopePVs]
	}
	for uid, ref := range ps.drain {
		p := m.idx.podByUID[uid]
		if p != nil && p.Owner != nil && !keep[p.Owner.String()] {
			continue
		}
		sc.DrainPods = append(sc.DrainPods, ref)
	}
	sortPodRefs(sc.DrainPods)
	if len(sc.DrainPods) > maxDrainPods {
		sc.DrainPods = sc.DrainPods[:maxDrainPods]
	}
	return sc
}

func sortPodRefs(refs []v1alpha1.PodRef) {
	sort.Slice(refs, func(i, j int) bool {
		a, b := refs[i], refs[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.UID < b.UID
	})
}

// triggersHealthy reports whether every triggering SmbMount is Healthy with
// no broken pod mount.
func (m *machine) triggersHealthy() bool {
	for _, name := range sortedUnique(m.obj.Spec.Trigger.SmbMounts) {
		sm := m.idx.mountByName[name]
		if sm == nil {
			continue
		}
		if sm.Status.Health != v1alpha1.HealthHealthy {
			return false
		}
		for _, pm := range sm.Status.PodMounts {
			if pm.Health == v1alpha1.HealthBroken {
				return false
			}
		}
	}
	return true
}

// conflictRefs is the set of workloads a scope conflict is checked against:
// the planned owners plus the owners of the triggering SmbMounts' pods that
// are already terminating. The planned scope skips terminating pods, but a pod
// terminates during a conflict precisely because another node's remediation
// drained its workload; without them this remediation would see no conflict,
// find no owner and end skipped_no_owner (backing the PV off) instead of
// waiting for the other remediation.
func (m *machine) conflictRefs(ps plannedScope) map[string]v1alpha1.WorkloadRef {
	refs := make(map[string]v1alpha1.WorkloadRef, len(ps.owners))
	for k, v := range ps.owners {
		refs[k] = v
	}
	for _, name := range sortedUnique(m.obj.Spec.Trigger.SmbMounts) {
		sm := m.idx.mountByName[name]
		if sm == nil {
			continue
		}
		// The worker reports a terminating pod's mount as Unknown (no longer
		// probed), so any terminating pod of a triggering SmbMount counts.
		for _, pm := range sm.Status.PodMounts {
			if p := m.idx.podByUID[pm.UID]; p != nil && p.Terminating && p.Owner != nil {
				refs[p.Owner.String()] = *p.Owner
			}
		}
	}
	return refs
}

// conflicts returns the names of other active remediations whose scope
// shares a workload with refs. seniorOnly limits it to remediations created
// before this one (tie-break by name).
func (m *machine) conflicts(refs map[string]v1alpha1.WorkloadRef, seniorOnly bool) []string {
	var out []string
	for _, o := range m.idx.others {
		if o.Status.Scope == nil {
			continue
		}
		if seniorOnly && !senior(&o, m.obj) {
			continue
		}
		for _, w := range o.Status.Scope.Workloads {
			if _, ok := refs[w.String()]; ok {
				out = append(out, o.Name)
				break
			}
		}
	}
	return out
}

func senior(a, b *v1alpha1.SmbMountRemediation) bool {
	ta, tb := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return a.Name < b.Name
}

// deadGlobalmounts returns the mounted, non-healthy (brokenOnly: Broken)
// globalmounts of pvs.
func (m *machine) deadGlobalmounts(pvs []string, brokenOnly bool) []v1alpha1.DeadGlobalmount {
	var out []v1alpha1.DeadGlobalmount
	for _, pv := range pvs {
		sm := m.idx.mountByPV[pv]
		if sm == nil || sm.Status.Globalmount == nil || !sm.Status.Globalmount.Mounted {
			continue
		}
		if brokenOnly && sm.Status.Health != v1alpha1.HealthBroken {
			continue
		}
		if !brokenOnly && sm.Status.Health == v1alpha1.HealthHealthy {
			continue
		}
		out = append(out, v1alpha1.DeadGlobalmount{PVName: pv, Path: sm.Spec.GlobalmountPath})
	}
	return out
}

// cotenantPVs are the scoped PVs whose globalmount is currently not healthy:
// every pod using them must leave the node before kubelet re-stages.
func (m *machine) cotenantPVs() []string {
	var out []string
	for _, pv := range m.scopePVs() {
		if !m.idx.globalmountHealthy(pv) {
			out = append(out, pv)
		}
	}
	return out
}

// ---------------- host tasks ----------------

type taskState int

const (
	taskNone      taskState = iota // nothing to do (no actions, no task)
	taskWaiting                    // created / running
	taskSucceeded                  // phase Succeeded
	taskFailed                     // phase Failed
	taskDryRun                     // dry run: task exists, not waited for
)

// ensureTask makes sure the host task (stage, seq) exists. The ref is
// recorded in status before the CreateHostTask command (write-ahead).
// actions is only evaluated while the task is not observed yet.
func (m *machine) ensureTask(stage v1alpha1.Stage, seq int, actions func() []v1alpha1.HostAction) taskState {
	name := v1alpha1.HostTaskName(m.obj.Name, stage, seq)
	if t := m.idx.hostTasks[name]; t != nil {
		m.recordTaskRef(name, stage, t.Status.Phase)
		if m.dry {
			return taskDryRun
		}
		switch t.Status.Phase {
		case v1alpha1.HostTaskSucceeded:
			return taskSucceeded
		case v1alpha1.HostTaskFailed:
			return taskFailed
		case v1alpha1.HostTaskPending, v1alpha1.HostTaskRunning, "":
		}
		return taskWaiting
	}
	acts := actions()
	if len(acts) == 0 {
		return taskNone
	}
	m.recordTaskRef(name, stage, "")
	m.emit(command.CreateHostTask{
		Name: name, NodeName: m.node, DryRun: m.dry, Actions: acts,
		Owner: command.OwnerRef{Name: m.obj.Name, UID: m.obj.UID},
	})
	return taskWaiting
}

func (m *machine) recordTaskRef(name string, stage v1alpha1.Stage, phase v1alpha1.HostTaskPhase) {
	for i := range m.st.HostTasks {
		if m.st.HostTasks[i].Name == name {
			if phase != "" {
				m.st.HostTasks[i].Phase = phase
			}
			return
		}
	}
	if len(m.st.HostTasks) >= maxHostTaskRefs {
		return
	}
	m.st.HostTasks = append(m.st.HostTasks, v1alpha1.HostTaskRef{Name: name, Stage: stage, Phase: phase})
}

// refreshHostTaskPhases copies observed phases into status.hostTasks.
func (m *machine) refreshHostTaskPhases() {
	for i := range m.st.HostTasks {
		if t := m.idx.hostTasks[m.st.HostTasks[i].Name]; t != nil && t.Status.Phase != "" {
			m.st.HostTasks[i].Phase = t.Status.Phase
		}
	}
}

// actionList builds HostActions with deterministic ids, capped at the CRD limit.
type actionList struct{ acts []v1alpha1.HostAction }

func (l *actionList) add(t v1alpha1.HostActionType, path, pv string, onlyIf v1alpha1.OnlyIf) {
	if path == "" || len(l.acts) >= maxHostActions {
		return
	}
	prefix := map[v1alpha1.HostActionType]string{
		v1alpha1.HostActionForceUnmount: "unmount",
		v1alpha1.HostActionQuarantine:   "quarantine",
		v1alpha1.HostActionProbeNow:     "probe",
	}[t]
	for _, a := range l.acts {
		if a.Type == t && a.Path == path {
			return
		}
	}
	l.acts = append(l.acts, v1alpha1.HostAction{
		ID: fmt.Sprintf("%s-%03d", prefix, len(l.acts)), Type: t, Path: path, PV: pv, OnlyIf: onlyIf,
	})
}

// probeGlobalmounts probes the globalmounts of pvs (nil: every SmbMount of the node).
func (m *machine) probeGlobalmounts(pvs []string) []v1alpha1.HostAction {
	var l actionList
	if pvs == nil {
		for _, n := range m.idx.mountNames {
			l.add(v1alpha1.HostActionProbeNow, m.idx.mountByName[n].Spec.GlobalmountPath, "", "")
		}
		return l.acts
	}
	for _, pv := range pvs {
		if sm := m.idx.mountByPV[pv]; sm != nil {
			l.add(v1alpha1.HostActionProbeNow, sm.Spec.GlobalmountPath, "", "")
		}
	}
	return l.acts
}

// ---------------- workloads ----------------

// holdScaledDown keeps every scoped workload at 0 replicas between draining
// and scaling_up (scale_down until first observed at 0, then scale_reassert).
func (m *machine) holdScaledDown() {
	for i := range m.scopeWorkloadsMut() {
		w := &m.st.Scope.Workloads[i]
		o, ok := m.idx.workload(w.WorkloadRef)
		switch {
		case ok && !o.Exists:
			w.State = v1alpha1.WorkloadDeleted
			continue
		case ok && o.Replicas == 0:
			if w.State != v1alpha1.WorkloadDeleted {
				w.State = v1alpha1.WorkloadScaledDown
			}
			continue
		}
		if m.dry {
			continue
		}
		action := command.ActionScaleReassert
		if w.State == v1alpha1.WorkloadInScope || w.State == "" {
			action = command.ActionScaleDown
		}
		m.emit(command.ScaleWorkload{Workload: w.WorkloadRef, Replicas: 0, Action: action})
	}
}

// ---------------- stages ----------------

func (m *machine) stepPending() {
	ps := m.planWorkloadScope()
	if names := m.conflicts(m.conflictRefs(ps), false); len(names) > 0 {
		setCondition(&m.st.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionScopeConflict, Status: metav1.ConditionTrue, Reason: "ScopeConflict",
			Message: "waiting for active remediation(s) " + strings.Join(names, ", "),
		}, m.nowS)
		return
	}
	waited := findCondition(m.st.Conditions, v1alpha1.ConditionScopeConflict) != nil
	if c := findCondition(m.st.Conditions, v1alpha1.ConditionScopeConflict); c != nil && c.Status == metav1.ConditionTrue {
		setCondition(&m.st.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionScopeConflict, Status: metav1.ConditionFalse, Reason: "Resolved",
			Message: "no conflicting remediation",
		}, m.nowS)
	}
	// The remediation it waited for drained (and replaced) this node's
	// broken pods. Planning now would find no owner and end
	// skipped_no_owner, backing the PV off; instead wait for the
	// replacements to break, or finish as a no-op once the mounts are fine.
	if waited && len(ps.owners) == 0 && len(ps.unowned) == 0 {
		if m.triggersHealthy() {
			m.finalize(ReasonSuperseded, v1alpha1.OutcomeHealed, "the findings cleared while waiting for another remediation")
		}
		return
	}
	m.move(v1alpha1.StagePlanning, ReasonNoScopeConflict)
}

func (m *machine) stepPlanningWorkload() {
	ts := m.ensureTask(v1alpha1.StagePlanning, 0, func() []v1alpha1.HostAction { return m.probeGlobalmounts(nil) })
	reason := ""
	switch {
	case m.dry:
		reason = ReasonDryRun
	case ts == taskSucceeded || ts == taskFailed:
		reason = ReasonProbeCompleted
	case m.deadlinePassed() || ts == taskNone:
		reason = ReasonProbeTimeout
	default:
		return
	}
	ps := m.planWorkloadScope()
	if names := m.conflicts(m.conflictRefs(ps), true); len(names) > 0 {
		m.st.Scope = nil
		setCondition(&m.st.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionScopeConflict, Status: metav1.ConditionTrue, Reason: "ScopeConflict",
			Message: "yielding to older remediation(s) " + strings.Join(names, ", "),
		}, m.nowS)
		m.move(v1alpha1.StagePending, ReasonScopeConflict)
		return
	}
	m.st.Scope = m.mergeScope(ps)
	if len(m.st.Scope.Workloads) == 0 {
		msg := "no Deployment/StatefulSet owns the broken pods; manual restart needed"
		if len(ps.unowned) > 0 {
			msg += ": " + strings.Join(ps.unowned, ", ")
		}
		m.finalize(ReasonNoOwner, v1alpha1.OutcomeSkippedNoOwner, msg)
		return
	}
	m.move(v1alpha1.StageDraining, reason)
}

func (m *machine) stepDraining() {
	m.extendScope()
	m.holdScaledDown()
	if m.dry {
		m.move(v1alpha1.StageReleasingMounts, ReasonDryRun)
		return
	}
	drain := m.drainUIDs()
	cot := m.cotenantPVs()
	var remaining []string
	for _, p := range m.idx.pods {
		using := false
		for _, pv := range cot {
			if containsString(p.PVs, pv) {
				using = true
			}
		}
		if drain[p.UID] || using {
			remaining = append(remaining, p.Namespace+"/"+p.Name)
		}
	}
	if len(remaining) == 0 {
		m.move(v1alpha1.StageReleasingMounts, ReasonDrained)
		return
	}
	if m.deadlinePassed() {
		m.event(command.EventWarning, "DrainTimeout", "Drain",
			"pods still present after the scale-down wait: "+strings.Join(remaining, ", "))
		m.move(v1alpha1.StageReleasingMounts, ReasonScaleDownTimeout)
	}
}

// extendScope adds owners of newly appearing pods on co-tenant PVs (and new
// pods of scoped workloads) to the scope while draining.
func (m *machine) extendScope() {
	if m.st.Scope == nil {
		return
	}
	cot := m.cotenantPVs()
	drain := m.drainUIDs()
	changed := false
	for i := range m.idx.pods {
		p := &m.idx.pods[i]
		if p.Owner == nil || drain[p.UID] {
			continue
		}
		uses := false
		for _, pv := range cot {
			if containsString(p.PVs, pv) {
				uses = true
			}
		}
		if !m.inScope(*p.Owner) {
			if !uses || len(m.st.Scope.Workloads) >= maxScopeWorkloads {
				continue
			}
			o, ok := m.idx.workload(*p.Owner)
			if !ok || !o.Exists {
				continue
			}
			m.st.Scope.Workloads = append(m.st.Scope.Workloads, v1alpha1.ScopedWorkload{
				WorkloadRef: *p.Owner, OriginalReplicas: o.Replicas, State: v1alpha1.WorkloadInScope,
			})
			sort.Slice(m.st.Scope.Workloads, func(a, b int) bool {
				return m.st.Scope.Workloads[a].String() < m.st.Scope.Workloads[b].String()
			})
		}
		if len(m.st.Scope.DrainPods) < maxDrainPods {
			m.st.Scope.DrainPods = append(m.st.Scope.DrainPods, v1alpha1.PodRef{Namespace: p.Namespace, Name: p.Name, UID: p.UID})
			drain[p.UID] = true
			changed = true
		}
	}
	if changed {
		sortPodRefs(m.st.Scope.DrainPods)
	}
}

func (m *machine) stepReleasingMounts() {
	m.holdScaledDown()
	ts := m.ensureTask(v1alpha1.StageReleasingMounts, 0, m.releaseActions)
	m.afterTask(ts, v1alpha1.StageAwaitingUnstage)
}

// afterTask moves to next once a host task finished (failures are best effort).
func (m *machine) afterTask(ts taskState, next v1alpha1.Stage) {
	switch ts {
	case taskNone:
		m.move(next, ReasonNothingToDo)
	case taskDryRun:
		m.move(next, ReasonDryRun)
	case taskSucceeded, taskFailed:
		m.move(next, ReasonHostTaskCompleted)
	case taskWaiting:
	}
}

// releaseActions force-unmounts every reported-mounted pod path of the drain
// pods on the scoped PVs.
func (m *machine) releaseActions() []v1alpha1.HostAction {
	drain := m.drainUIDs()
	var l actionList
	for _, pv := range m.scopePVs() {
		sm := m.idx.mountByPV[pv]
		if sm == nil {
			continue
		}
		var paths []string
		for _, pm := range sm.Status.PodMounts {
			if drain[pm.UID] && pm.Mounted {
				paths = append(paths, podVolumePath(sm, pm.UID))
			}
		}
		for _, od := range sm.Status.OrphanPodDirs {
			if drain[od.UID] && od.Mounted {
				paths = append(paths, od.Path)
			}
		}
		for _, p := range sortedUnique(paths) {
			l.add(v1alpha1.HostActionForceUnmount, p, pv, v1alpha1.OnlyIfNotHealthyMount)
		}
	}
	return l.acts
}

func (m *machine) stepAwaitingUnstage() {
	m.holdScaledDown()
	if len(m.st.DeadGlobalmounts) == 0 {
		m.move(v1alpha1.StageQuarantining, ReasonNothingToDo)
		return
	}
	// Once the force-unmount task (created after UNSTAGE_WAIT) exists, its
	// completion decides; otherwise kubelet's own unstage does.
	if t := m.idx.hostTasks[v1alpha1.HostTaskName(m.obj.Name, v1alpha1.StageAwaitingUnstage, 0)]; t != nil && !m.dry {
		m.recordTaskRef(t.Name, v1alpha1.StageAwaitingUnstage, t.Status.Phase)
		if t.Status.Phase == v1alpha1.HostTaskSucceeded || t.Status.Phase == v1alpha1.HostTaskFailed {
			m.move(v1alpha1.StageQuarantining, ReasonForceUnmounted)
			return
		}
	}
	var mounted []v1alpha1.DeadGlobalmount
	for _, d := range m.st.DeadGlobalmounts {
		if sm := m.idx.mountByPV[d.PVName]; sm != nil && sm.Status.Globalmount != nil && sm.Status.Globalmount.Mounted {
			mounted = append(mounted, d)
		}
	}
	if len(mounted) == 0 {
		m.move(v1alpha1.StageQuarantining, ReasonUnstaged)
		return
	}
	if !m.dry && !m.deadlinePassed() {
		return
	}
	ts := m.ensureTask(v1alpha1.StageAwaitingUnstage, 0, func() []v1alpha1.HostAction {
		var l actionList
		for _, d := range mounted {
			l.add(v1alpha1.HostActionForceUnmount, d.Path, d.PVName, v1alpha1.OnlyIfNotHealthyMount)
		}
		return l.acts
	})
	switch ts {
	case taskNone:
		m.move(v1alpha1.StageQuarantining, ReasonNothingToDo)
	case taskDryRun:
		m.move(v1alpha1.StageQuarantining, ReasonDryRun)
	case taskSucceeded, taskFailed:
		m.move(v1alpha1.StageQuarantining, ReasonForceUnmounted)
	case taskWaiting:
	}
}

func (m *machine) stepQuarantining() {
	m.holdScaledDown()
	ts := m.ensureTask(v1alpha1.StageQuarantining, 0, m.quarantineActions)
	m.afterTask(ts, v1alpha1.StageAwaitingUnstageCompletion)
}

// quarantineActions covers the pod dirs of any pod UID and the globalmount
// dir of every scoped PV: ForceUnmount mounted paths (onlyIf=NotHealthyMount)
// and Quarantine every dir (onlyIf=NotMountpoint; empty dirs are skipped by
// the worker).
func (m *machine) quarantineActions() []v1alpha1.HostAction {
	var l actionList
	for _, pv := range m.scopePVs() {
		sm := m.idx.mountByPV[pv]
		if sm == nil {
			continue
		}
		mounted := map[string]bool{}
		var paths []string
		for _, pm := range sm.Status.PodMounts {
			p := podVolumePath(sm, pm.UID)
			paths = append(paths, p)
			mounted[p] = mounted[p] || pm.Mounted
		}
		for _, od := range sm.Status.OrphanPodDirs {
			paths = append(paths, od.Path)
			mounted[od.Path] = mounted[od.Path] || od.Mounted
		}
		for _, p := range sortedUnique(paths) {
			if mounted[p] {
				l.add(v1alpha1.HostActionForceUnmount, p, pv, v1alpha1.OnlyIfNotHealthyMount)
			}
			l.add(v1alpha1.HostActionQuarantine, p, pv, v1alpha1.OnlyIfNotMountpoint)
		}
		g := sm.Status.Globalmount
		if g != nil && g.Mounted {
			l.add(v1alpha1.HostActionForceUnmount, sm.Spec.GlobalmountPath, pv, v1alpha1.OnlyIfNotHealthyMount)
		}
		if g == nil || g.DirExists || g.Mounted {
			l.add(v1alpha1.HostActionQuarantine, sm.Spec.GlobalmountPath, pv, v1alpha1.OnlyIfNotMountpoint)
		}
	}
	return l.acts
}

func (m *machine) stepRestartingDriver() {
	m.holdScaledDown()
	if m.dry {
		m.move(v1alpha1.StageScalingUp, ReasonDryRun)
		return
	}
	if m.st.Driver == nil || m.st.Driver.OldPodUID == "" {
		if len(m.idx.driverPods) == 0 {
			m.move(v1alpha1.StageScalingUp, ReasonNoDriverPod)
			return
		}
		old := m.idx.driverPods[0]
		// Write-ahead: the old UID is in the returned status, which the
		// caller persists before executing the DeletePod below.
		m.st.Driver = &v1alpha1.DriverRestart{OldPodNamespace: old.Namespace, OldPodName: old.Name, OldPodUID: old.UID}
	}
	d := m.st.Driver
	for _, p := range m.idx.driverPods {
		if p.UID == d.OldPodUID {
			m.emit(command.DeletePod{Namespace: d.OldPodNamespace, Name: d.OldPodName, UID: d.OldPodUID, Action: command.ActionDriverRestart})
			break
		}
	}
	for _, p := range m.idx.driverPods {
		if p.UID != d.OldPodUID && p.Running && p.Ready {
			d.NewPodUID = p.UID
			m.move(v1alpha1.StageScalingUp, ReasonDriverReady)
			return
		}
	}
	if m.deadlinePassed() {
		m.move(v1alpha1.StageScalingUp, ReasonDriverRestartTimeout)
	}
}

func (m *machine) stepScalingUp() {
	if m.dry {
		m.move(v1alpha1.StageVerifying, ReasonDryRun)
		return
	}
	done := true
	for i := range m.scopeWorkloadsMut() {
		w := &m.st.Scope.Workloads[i]
		if w.State == v1alpha1.WorkloadRestored || w.State == v1alpha1.WorkloadDeleted {
			continue
		}
		o, ok := m.idx.workload(w.WorkloadRef)
		switch {
		case ok && !o.Exists:
			w.State = v1alpha1.WorkloadDeleted
		case ok && o.Replicas == w.OriginalReplicas:
			w.State = v1alpha1.WorkloadRestored
		default:
			m.emit(command.ScaleWorkload{Workload: w.WorkloadRef, Replicas: w.OriginalReplicas, Action: command.ActionScaleUp})
			done = false
		}
	}
	if done {
		m.move(v1alpha1.StageVerifying, ReasonScaledUp)
	}
}

func (m *machine) stepVerifying() {
	if m.dry {
		m.move(v1alpha1.StageFinalizing, ReasonDryRun)
		return
	}
	m.ensureTask(v1alpha1.StageVerifying, 0, func() []v1alpha1.HostAction { return m.probeGlobalmounts(m.scopePVs()) })
	drain := m.drainUIDs()
	graceOver := m.st.StageEnteredTime != nil && !m.now.Before(m.st.StageEnteredTime.Add(m.cfg.VerifyNoPodGrace))
	var unverifiedPods []*PodObservation
	pending := 0
	for i := range m.st.Verification {
		v := &m.st.Verification[i]
		if v.Result != v1alpha1.VerificationPending {
			continue
		}
		if o, ok := m.idx.workload(v.Workload); ok && !o.Exists {
			v.Result = v1alpha1.VerificationElsewhere
			continue
		}
		var fresh []*PodObservation
		for j := range m.idx.pods {
			p := &m.idx.pods[j]
			if p.Owner != nil && *p.Owner == v.Workload && !drain[p.UID] && !p.Terminating {
				fresh = append(fresh, p)
			}
		}
		if len(fresh) == 0 {
			if graceOver {
				v.Result = v1alpha1.VerificationElsewhere
				continue
			}
			pending++
			continue
		}
		healthy := true
		for _, p := range fresh {
			if !m.podHealthy(p) {
				healthy = false
				unverifiedPods = append(unverifiedPods, p)
			}
		}
		if healthy {
			v.Result = v1alpha1.VerificationVerified
			continue
		}
		pending++
	}
	if len(unverifiedPods) > 0 {
		m.ensureTask(v1alpha1.StageVerifying, 1, func() []v1alpha1.HostAction {
			var l actionList
			for _, p := range unverifiedPods {
				for _, pv := range p.PVs {
					if sm := m.idx.mountByPV[pv]; sm != nil {
						l.add(v1alpha1.HostActionProbeNow, podVolumePath(sm, p.UID), "", "")
					}
				}
			}
			return l.acts
		})
	}
	if pending == 0 {
		m.finalize(ReasonVerified, m.defaultOutcome(), "")
		return
	}
	if m.deadlinePassed() {
		for i := range m.st.Verification {
			if m.st.Verification[i].Result == v1alpha1.VerificationPending {
				m.st.Verification[i].Result = v1alpha1.VerificationFailed
			}
		}
		m.finalize(ReasonVerifyTimeout, m.defaultOutcome(), "")
	}
}

// podHealthy reports whether every smb PV of p has a Healthy podMount for p's UID.
func (m *machine) podHealthy(p *PodObservation) bool {
	if len(p.PVs) == 0 {
		return false
	}
	for _, pv := range p.PVs {
		sm := m.idx.mountByPV[pv]
		if sm == nil {
			return false
		}
		ok := false
		for _, pm := range sm.Status.PodMounts {
			if pm.UID == p.UID && pm.Health == v1alpha1.HealthHealthy {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
