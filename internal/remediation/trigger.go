package remediation

import (
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// BackoffWindow is how far back terminal remediations count towards backoff
// (extended to BackoffMax if that is longer).
const BackoffWindow = time.Hour

type finding struct {
	name    string
	pv      string
	reasons []v1alpha1.TriggerReason
	owners  map[string]bool
}

// Trigger decides whether a new remediation should be created for in.NodeName.
//
//   - Workload: some podMount is Broken on an existing, non-terminating pod.
//   - Sweep: no workload finding, but orphan findings on PVs no pod on the
//     node references (see orphanFindings).
//   - At most one non-terminal remediation per node (active_remediation).
//   - Backoff per (node, PV): k terminal remediations with the same dryRun
//     mode touching the PV finished within the window suppress until
//     lastFinished + BackoffBase*2^(k-1), capped at BackoffMax.
//   - Scope conflict: a broken pod's owner is in the scope of an active
//     remediation on another node.
//
// Suppressed lists every SmbMount with a finding that was not included.
func Trigger(in TriggerInput, cfg Config) TriggerDecision {
	dec := TriggerDecision{Suppressed: map[string]SuppressionReason{}}

	var mounts []*v1alpha1.SmbMount
	seen := map[string]bool{}
	for i := range in.SmbMounts {
		sm := &in.SmbMounts[i]
		if (sm.Spec.NodeName != "" && sm.Spec.NodeName != in.NodeName) || seen[sm.Name] {
			continue
		}
		seen[sm.Name] = true
		mounts = append(mounts, sm)
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Name < mounts[j].Name })

	pods := sortedPods(in.Pods, in.NodeName)
	podByUID := map[types.UID]*PodObservation{}
	referenced := map[string]bool{}
	for i := range pods {
		podByUID[pods[i].UID] = &pods[i]
		for _, pv := range pods[i].PVs {
			referenced[pv] = true
		}
	}

	var work, sweep []finding
	for _, sm := range mounts {
		f := finding{name: sm.Name, pv: sm.Spec.PVName, owners: map[string]bool{}}
		for _, pm := range sm.Status.PodMounts {
			if pm.Health != v1alpha1.HealthBroken {
				continue
			}
			p := podByUID[pm.UID]
			if p == nil || p.Terminating {
				continue
			}
			if r := triggerReason(pm.Reason); r != "" {
				f.reasons = append(f.reasons, r)
			}
			if p.Owner != nil {
				f.owners[p.Owner.String()] = true
			}
			if len(f.reasons) == 0 {
				f.reasons = append(f.reasons, "") // finding without a known reason
			}
		}
		if len(f.reasons) > 0 {
			work = append(work, f)
			continue
		}
		if !referenced[sm.Spec.PVName] {
			if rs := orphanFindings(sm); len(rs) > 0 {
				f.reasons = rs
				sweep = append(sweep, f)
			}
		}
	}

	typ := v1alpha1.RemediationTypeWorkload
	findings := work
	if len(work) == 0 {
		typ, findings = v1alpha1.RemediationTypeSweep, sweep
	}
	if len(findings) == 0 {
		return dec
	}

	var others []v1alpha1.SmbMountRemediation
	for _, r := range in.Remediations {
		if r.Status.Stage.Terminal() {
			continue
		}
		if r.Spec.NodeName == in.NodeName {
			for _, f := range findings {
				dec.Suppressed[f.name] = v1alpha1.SuppressionActiveRemediation
			}
			return dec
		}
		others = append(others, r)
	}

	var names []string
	var reasons []v1alpha1.TriggerReason
	for _, f := range findings {
		if suppressedByBackoff(in, cfg, f) {
			dec.Suppressed[f.name] = v1alpha1.SuppressionBackoff
			continue
		}
		if typ == v1alpha1.RemediationTypeWorkload && ownersConflict(f.owners, others) {
			dec.Suppressed[f.name] = v1alpha1.SuppressionScopeConflict
			continue
		}
		names = append(names, f.name)
		for _, r := range f.reasons {
			if r != "" {
				reasons = append(reasons, r)
			}
		}
	}
	if len(names) == 0 {
		return dec
	}
	rs := make([]string, len(reasons))
	for i, r := range reasons {
		rs[i] = string(r)
	}
	rs = sortedUnique(rs)
	reasons = reasons[:0]
	for _, r := range rs {
		reasons = append(reasons, v1alpha1.TriggerReason(r))
	}
	dec.Create = &v1alpha1.SmbMountRemediation{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "SmbMountRemediation"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       v1alpha1.RemediationName(in.NodeName, in.Now),
			Labels:     map[string]string{v1alpha1.LabelNode: in.NodeName},
			Finalizers: []string{v1alpha1.FinalizerRestoreReplicas},
		},
		Spec: v1alpha1.SmbMountRemediationSpec{
			NodeName: in.NodeName,
			Type:     typ,
			Trigger:  v1alpha1.RemediationTrigger{SmbMounts: sortedUnique(names), Reasons: reasons},
			DryRun:   cfg.DryRun,
		},
	}
	return dec
}

func triggerReason(r v1alpha1.FailureReason) v1alpha1.TriggerReason {
	switch r {
	case v1alpha1.ReasonNotMountpoint:
		return v1alpha1.TriggerNotMountpoint
	case v1alpha1.ReasonWrongFSType:
		return v1alpha1.TriggerWrongFSType
	case v1alpha1.ReasonStatError:
		return v1alpha1.TriggerStatError
	case v1alpha1.ReasonStatTimeout:
		return v1alpha1.TriggerStatTimeout
	case v1alpha1.ReasonProbeHung:
		return v1alpha1.TriggerProbeHung
	case v1alpha1.ReasonProbeError:
	}
	return ""
}

func ownersConflict(owners map[string]bool, others []v1alpha1.SmbMountRemediation) bool {
	for _, o := range others {
		if o.Status.Scope == nil {
			continue
		}
		for _, w := range o.Status.Scope.Workloads {
			if owners[w.String()] {
				return true
			}
		}
	}
	return false
}

// Backoff returns the suppression duration after k recent remediations:
// BackoffBase*2^(k-1), capped at BackoffMax; 0 for k <= 0.
func Backoff(k int, cfg Config) time.Duration {
	if k <= 0 || cfg.BackoffBase <= 0 {
		return 0
	}
	d := cfg.BackoffBase
	for i := 1; i < k; i++ {
		if cfg.BackoffMax > 0 && d >= cfg.BackoffMax {
			break
		}
		if d > time.Duration(1<<62) {
			break
		}
		d *= 2
	}
	if cfg.BackoffMax > 0 && d > cfg.BackoffMax {
		d = cfg.BackoffMax
	}
	return d
}

// finishedTime is when a terminal remediation reached done.
func finishedTime(r *v1alpha1.SmbMountRemediation) time.Time {
	if r.Status.StageEnteredTime != nil {
		return r.Status.StageEnteredTime.Time
	}
	if n := len(r.Status.Transitions); n > 0 {
		return r.Status.Transitions[n-1].Time.Time
	}
	return r.CreationTimestamp.Time
}

// noOp reports whether a terminal remediation finished without acting on the
// host or on workloads: a Sweep whose fresh probe left nothing to sweep, or a
// Workload remediation superseded by another node's. Such an episode must not
// count towards backoff: it would delay the remediation the finding needs.
func noOp(r *v1alpha1.SmbMountRemediation) bool {
	for _, t := range r.Status.Transitions {
		if t.To == v1alpha1.StageFinalizing && (t.Reason == ReasonNothingToSweep || t.Reason == ReasonSuperseded) {
			return true
		}
	}
	return false
}

func touches(r *v1alpha1.SmbMountRemediation, name, pv string) bool {
	for _, n := range r.Spec.Trigger.SmbMounts {
		if n == name {
			return true
		}
	}
	return r.Status.Scope != nil && containsString(r.Status.Scope.PVs, pv)
}

func suppressedByBackoff(in TriggerInput, cfg Config, f finding) bool {
	window := BackoffWindow
	if cfg.BackoffMax > window {
		window = cfg.BackoffMax
	}
	k := 0
	var last time.Time
	for i := range in.Remediations {
		r := &in.Remediations[i]
		if r.Spec.NodeName != in.NodeName || !r.Status.Stage.Terminal() || r.Spec.DryRun != cfg.DryRun {
			continue
		}
		if !touches(r, f.name, f.pv) || noOp(r) {
			continue
		}
		fin := finishedTime(r)
		if fin.After(in.Now) || in.Now.Sub(fin) > window {
			continue
		}
		k++
		if fin.After(last) {
			last = fin
		}
	}
	if k == 0 {
		return false
	}
	return in.Now.Before(last.Add(Backoff(k, cfg)))
}
