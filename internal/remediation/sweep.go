package remediation

import (
	"sort"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

func (m *machine) stepSweep() {
	switch s := m.st.Stage; s {
	case v1alpha1.StagePlanning:
		m.stepPlanningSweep()
	case v1alpha1.StageSweeping:
		m.stepSweeping()
	case v1alpha1.StageAwaitingUnstageCompletion, v1alpha1.StageRollingBack,
		v1alpha1.StageFinalizing, v1alpha1.StageDone:
		m.stepCommon(s)
	case v1alpha1.StagePending, v1alpha1.StageDraining, v1alpha1.StageReleasingMounts,
		v1alpha1.StageAwaitingUnstage, v1alpha1.StageQuarantining, v1alpha1.StageRestartingDriver,
		v1alpha1.StageScalingUp, v1alpha1.StageVerifying:
		// Not a Sweep stage; roll back defensively.
		m.move(v1alpha1.StageRollingBack, ReasonAborted)
	}
}

// orphanFindings returns the sweep trigger reasons of one SmbMount whose PV
// no pod on the node references. A mounted orphan pod dir is always stale
// (its pod is gone), so it counts as "mounted and unhealthy".
func orphanFindings(sm *v1alpha1.SmbMount) []v1alpha1.TriggerReason {
	var out []v1alpha1.TriggerReason
	if g := sm.Status.Globalmount; g != nil {
		if g.Mounted && sm.Status.Health == v1alpha1.HealthBroken {
			out = append(out, v1alpha1.TriggerOrphanGlobalmountBroken)
		}
		if !g.Mounted && g.DirExists && !g.Empty {
			out = append(out, v1alpha1.TriggerGlobalmountNotEmpty)
		}
	}
	var notEmpty, unhealthy bool
	for _, od := range sm.Status.OrphanPodDirs {
		if od.Mounted {
			unhealthy = true
		} else if !od.Empty {
			notEmpty = true
		}
	}
	if notEmpty {
		out = append(out, v1alpha1.TriggerOrphanPodDirNotEmpty)
	}
	if unhealthy {
		out = append(out, v1alpha1.TriggerOrphanPodDirUnhealthy)
	}
	return out
}

func (m *machine) stepPlanningSweep() {
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
	var pvs []string
	for _, name := range sortedUnique(m.obj.Spec.Trigger.SmbMounts) {
		sm := m.idx.mountByName[name]
		if sm == nil || m.idx.pvReferenced(sm.Spec.PVName) {
			continue
		}
		if len(orphanFindings(sm)) > 0 {
			pvs = append(pvs, sm.Spec.PVName)
		}
	}
	pvs = sortedUnique(pvs)
	if len(pvs) == 0 {
		m.st.Scope = nil
		m.finalize(ReasonNothingToSweep, v1alpha1.OutcomeHealed, "no orphan finding left after a fresh probe")
		return
	}
	if len(pvs) > maxScopePVs {
		pvs = pvs[:maxScopePVs]
	}
	m.st.Scope = &v1alpha1.RemediationScope{PVs: pvs}
	m.st.DeadGlobalmounts = m.deadGlobalmounts(pvs, true)
	m.move(v1alpha1.StageSweeping, reason)
}

func (m *machine) stepSweeping() {
	ts := m.ensureTask(v1alpha1.StageSweeping, 0, m.sweepActions)
	switch ts {
	case taskNone:
		m.move(v1alpha1.StageAwaitingUnstageCompletion, ReasonNothingToDo)
	case taskDryRun:
		m.move(v1alpha1.StageAwaitingUnstageCompletion, ReasonDryRun)
	case taskSucceeded:
		m.move(v1alpha1.StageAwaitingUnstageCompletion, ReasonHostTaskCompleted)
	case taskFailed:
		m.finalize(ReasonHostTaskFailed, v1alpha1.OutcomeFailed, "sweep host task failed")
	case taskWaiting:
	}
}

// sweepActions: ForceUnmount dead orphan globalmounts and mounted orphan pod
// dirs, Quarantine orphan pod dirs and globalmount dirs. Never touches a PV
// a pod on the node references.
func (m *machine) sweepActions() []v1alpha1.HostAction {
	dead := map[string]bool{}
	for _, d := range m.st.DeadGlobalmounts {
		dead[d.PVName] = true
	}
	var l actionList
	for _, pv := range m.scopePVs() {
		sm := m.idx.mountByPV[pv]
		if sm == nil || m.idx.pvReferenced(pv) {
			continue
		}
		dirs := append([]v1alpha1.OrphanPodDir(nil), sm.Status.OrphanPodDirs...)
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Path < dirs[j].Path })
		for _, od := range dirs {
			if od.Mounted {
				l.add(v1alpha1.HostActionForceUnmount, od.Path, pv, v1alpha1.OnlyIfNotHealthyMount)
			}
			if od.Mounted || !od.Empty {
				l.add(v1alpha1.HostActionQuarantine, od.Path, pv, v1alpha1.OnlyIfNotMountpoint)
			}
		}
		g := sm.Status.Globalmount
		if dead[pv] {
			l.add(v1alpha1.HostActionForceUnmount, sm.Spec.GlobalmountPath, pv, v1alpha1.OnlyIfNotHealthyMount)
			l.add(v1alpha1.HostActionQuarantine, sm.Spec.GlobalmountPath, pv, v1alpha1.OnlyIfNotMountpoint)
		} else if g != nil && !g.Mounted && g.DirExists && !g.Empty {
			l.add(v1alpha1.HostActionQuarantine, sm.Spec.GlobalmountPath, pv, v1alpha1.OnlyIfNotMountpoint)
		}
	}
	return l.acts
}
