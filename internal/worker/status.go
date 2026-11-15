package worker

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	ac "github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/health"
	"github.com/marcherschel/smb-mount-healer/internal/inventory"
)

// Condition reasons written by the worker.
const (
	ReasonHealthy           = "Healthy"
	ReasonNotProbed         = "NotProbed"
	ReasonQuarantinePending = "QuarantinePending"
	ReasonNoQuarantine      = "NoQuarantine"
	ReasonDataPresent       = "DataPresent"
)

// workerConditions are the SmbMount condition types the worker owns.
var workerConditions = map[string]bool{
	v1alpha1.ConditionHealthy:           true,
	v1alpha1.ConditionShadowDataPending: true,
}

// smbMountSpec is the immutable spec of vol's SmbMount on node.
func smbMountSpec(node string, vol inventory.VolumeView) v1alpha1.SmbMountSpec {
	return v1alpha1.SmbMountSpec{
		NodeName:        node,
		PVName:          vol.PVName,
		Share:           vol.Share,
		VolumeHandle:    vol.VolumeHandle,
		GlobalmountPath: vol.Globalmount.Path,
	}
}

// workerStatus builds the worker-owned SmbMount status fields of vol.
//
// Top-level health, reason and debounce are the globalmount's FSM record
// while the globalmount is mounted (the control plane reads them as
// "globalmount health", always gated on globalmount.mounted). Otherwise
// health and reason are the worst pod-mount health (display only, no
// debounce), or Unknown. prev supplies condition lastTransitionTimes.
func workerStatus(vol inventory.VolumeView, records map[string]health.Record, pending int32,
	at time.Time, prev []metav1.Condition, now time.Time,
) v1alpha1.SmbMountStatus {
	st := v1alpha1.SmbMountStatus{PendingQuarantines: pending}
	lpt := metav1.NewTime(at)
	st.LastProbeTime = &lpt

	gm := vol.Globalmount
	st.Globalmount = &v1alpha1.GlobalmountStatus{
		Mounted:      gm.Mounted,
		FSType:       gm.FSType,
		SuperOptions: gm.SuperOptions,
		DirExists:    gm.Mounted || gm.Dir.Exists,
		Empty:        !gm.Mounted && gm.Dir.Exists && gm.Dir.Empty,
	}

	top := health.Record{State: v1alpha1.HealthUnknown}
	if r, ok := records[gm.Path]; ok && gm.Mounted {
		top = r
	}
	for _, pm := range vol.PodMounts {
		ps := v1alpha1.PodMountStatus{
			Namespace: pm.Namespace,
			Pod:       pm.Pod,
			UID:       pm.UID,
			Workload:  pm.Workload,
			Mounted:   pm.Mounted,
			FSType:    pm.FSType,
			Health:    v1alpha1.HealthUnknown,
		}
		if r, ok := records[pm.Path]; ok && !pm.Skip {
			ps.Health, ps.Reason = recordHealth(r)
		}
		if !gm.Mounted && !pm.Skip && severity(ps.Health) > severity(top.State) {
			top = health.Record{State: ps.Health, Reason: ps.Reason}
		}
		st.PodMounts = append(st.PodMounts, ps)
	}
	if gm.Mounted {
		st.Health, st.Reason, st.Debounce = top.ToStatus()
	} else {
		st.Health, st.Reason = recordHealth(top)
	}
	for _, od := range vol.OrphanDirs {
		st.OrphanPodDirs = append(st.OrphanPodDirs, v1alpha1.OrphanPodDir{
			UID:     od.UID,
			Path:    od.Path,
			Mounted: od.Mounted,
			Empty:   !od.Mounted && (!od.Dir.Exists || od.Dir.Empty),
		})
	}

	healthy := metav1.Condition{Type: v1alpha1.ConditionHealthy}
	switch st.Health {
	case v1alpha1.HealthHealthy:
		healthy.Status, healthy.Reason, healthy.Message = metav1.ConditionTrue, ReasonHealthy, "all probed mounts are healthy"
	case v1alpha1.HealthUnknown:
		healthy.Status, healthy.Reason, healthy.Message = metav1.ConditionUnknown, ReasonNotProbed, "no mount of this volume has been probed"
	case v1alpha1.HealthSuspect, v1alpha1.HealthBroken, v1alpha1.HealthErrored:
		healthy.Status, healthy.Reason = metav1.ConditionFalse, string(st.Reason)
		healthy.Message = fmt.Sprintf("mount health is %s (%s)", st.Health, st.Reason)
		if st.Reason == "" {
			healthy.Reason = string(st.Health)
		}
	}
	shadow := metav1.Condition{Type: v1alpha1.ConditionShadowDataPending, Status: metav1.ConditionFalse,
		Reason: ReasonNoQuarantine, Message: "no quarantined data for this volume on this node"}
	if pending > 0 {
		shadow.Status, shadow.Reason = metav1.ConditionTrue, ReasonQuarantinePending
		shadow.Message = fmt.Sprintf("%d quarantine folder(s) hold shadow data of this volume; see kubectl get smbq", pending)
	}
	for _, c := range []metav1.Condition{healthy, shadow} {
		c.LastTransitionTime = metav1.NewTime(now)
		for _, old := range prev {
			if old.Type == c.Type && old.Status == c.Status && !old.LastTransitionTime.IsZero() {
				c.LastTransitionTime = old.LastTransitionTime
			}
		}
		st.Conditions = append(st.Conditions, c)
	}
	return st
}

func recordHealth(r health.Record) (v1alpha1.Health, v1alpha1.FailureReason) {
	h, reason, _ := r.ToStatus()
	if h == v1alpha1.HealthHealthy || h == v1alpha1.HealthUnknown {
		reason = ""
	}
	return h, reason
}

// workerFields keeps only the worker-owned fields of a live status.
func workerFields(s v1alpha1.SmbMountStatus) v1alpha1.SmbMountStatus {
	out := v1alpha1.SmbMountStatus{
		Health:             s.Health,
		Reason:             s.Reason,
		LastProbeTime:      s.LastProbeTime,
		Debounce:           s.Debounce,
		Globalmount:        s.Globalmount,
		PodMounts:          s.PodMounts,
		OrphanPodDirs:      s.OrphanPodDirs,
		PendingQuarantines: s.PendingQuarantines,
	}
	for _, c := range s.Conditions {
		if workerConditions[c.Type] {
			out.Conditions = append(out.Conditions, c)
		}
	}
	return out
}

// fingerprint identifies the content of a worker status, ignoring
// lastProbeTime and condition transition times (which change on their own).
// JSON gives metav1.Time second precision, matching what the API stores.
func fingerprint(s v1alpha1.SmbMountStatus) string {
	s = workerFields(s)
	s.LastProbeTime = nil
	s.PodMounts = slices.Clone(s.PodMounts)
	slices.SortFunc(s.PodMounts, func(a, b v1alpha1.PodMountStatus) int { return cmp.Compare(a.UID, b.UID) })
	s.OrphanPodDirs = slices.Clone(s.OrphanPodDirs)
	slices.SortFunc(s.OrphanPodDirs, func(a, b v1alpha1.OrphanPodDir) int { return cmp.Compare(a.Path, b.Path) })
	conds := make([]metav1.Condition, len(s.Conditions))
	for i, c := range s.Conditions {
		conds[i] = metav1.Condition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message}
	}
	slices.SortFunc(conds, func(a, b metav1.Condition) int { return cmp.Compare(a.Type, b.Type) })
	s.Conditions = conds
	b, err := json.Marshal(s)
	if err != nil {
		return "" // cannot happen for this type; "" forces a write
	}
	return string(b)
}

// statusApply converts the worker fields of st into an apply configuration.
// Only fields set here are owned by FieldManagerWorker; the control plane's
// fields are never mentioned.
func statusApply(st v1alpha1.SmbMountStatus) *ac.SmbMountStatusApplyConfiguration {
	a := ac.SmbMountStatus()
	if st.Health != "" {
		a.WithHealth(st.Health)
	}
	if st.Reason != "" {
		a.WithReason(st.Reason)
	}
	if st.LastProbeTime != nil {
		a.WithLastProbeTime(*st.LastProbeTime)
	}
	if d := st.Debounce; d != nil {
		da := ac.Debounce().WithConsecutiveFailures(d.ConsecutiveFailures)
		if d.FirstFailureTime != nil {
			da.WithFirstFailureTime(*d.FirstFailureTime)
		}
		if d.LastClass != "" {
			da.WithLastClass(d.LastClass)
		}
		if d.RecoveringSince != nil {
			da.WithRecoveringSince(*d.RecoveringSince)
		}
		a.WithDebounce(da)
	}
	if g := st.Globalmount; g != nil {
		ga := ac.GlobalmountStatus().WithMounted(g.Mounted).WithDirExists(g.DirExists).WithEmpty(g.Empty)
		if g.FSType != "" {
			ga.WithFSType(g.FSType)
		}
		if g.SuperOptions != "" {
			ga.WithSuperOptions(g.SuperOptions)
		}
		a.WithGlobalmount(ga)
	}
	for _, pm := range st.PodMounts {
		pa := ac.PodMountStatus().WithNamespace(pm.Namespace).WithPod(pm.Pod).WithUID(pm.UID).WithMounted(pm.Mounted)
		if pm.Workload != "" {
			pa.WithWorkload(pm.Workload)
		}
		if pm.FSType != "" {
			pa.WithFSType(pm.FSType)
		}
		if pm.Health != "" {
			pa.WithHealth(pm.Health)
		}
		if pm.Reason != "" {
			pa.WithReason(pm.Reason)
		}
		a.WithPodMounts(pa)
	}
	for _, od := range st.OrphanPodDirs {
		a.WithOrphanPodDirs(ac.OrphanPodDir().WithUID(od.UID).WithPath(od.Path).WithMounted(od.Mounted).WithEmpty(od.Empty))
	}
	if st.PendingQuarantines > 0 {
		a.WithPendingQuarantines(st.PendingQuarantines)
	}
	for _, c := range st.Conditions {
		if !workerConditions[c.Type] {
			continue
		}
		a.WithConditions(metav1ac.Condition().WithType(c.Type).WithStatus(c.Status).WithReason(c.Reason).
			WithMessage(c.Message).WithLastTransitionTime(c.LastTransitionTime))
	}
	return a
}
