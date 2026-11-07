package controlplane

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/remediation"
)

// remediationsWindow is the window of SmbMount.status.remediationsLastHour.
const remediationsWindow = time.Hour

// ReconcileTrigger evaluates one node: it creates an SmbMountRemediation
// when remediation.Trigger asks for one, records suppressions (condition
// RemediationSuppressed on the SmbMount and remediation_suppressed_total)
// and keeps remediationsLastHour current. It also refreshes the
// smbmounts{health} gauge.
func (e *Engine) ReconcileTrigger(ctx context.Context, node string) (reconcile.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("node", node)
	ctx = logr.NewContext(ctx, log)
	if err := e.RefreshSmbMountGauge(ctx); err != nil {
		return reconcile.Result{}, err
	}
	mounts, err := e.nodeSmbMounts(ctx, node)
	if err != nil {
		return reconcile.Result{}, err
	}
	var rems v1alpha1.SmbMountRemediationList
	if err := e.c.List(ctx, &rems); err != nil {
		return reconcile.Result{}, fmt.Errorf("list remediations: %w", err)
	}
	now := e.clock.Now()
	in := remediation.TriggerInput{NodeName: node, Now: now, SmbMounts: mounts, Remediations: rems.Items}
	if anyFinding(mounts) {
		if in.Pods, err = e.nodePods(ctx, node, mounts); err != nil {
			return reconcile.Result{}, err
		}
	}
	dec := remediation.Trigger(in, e.rcfg)

	var requeue time.Duration
	setRequeue := func(d time.Duration) {
		if d > 0 && (requeue == 0 || d < requeue) {
			requeue = d
		}
	}
	if dec.Create != nil {
		d, err := e.createRemediation(ctx, dec.Create)
		if err != nil {
			return reconcile.Result{}, err
		}
		setRequeue(d)
	}

	active := activeOn(rems.Items, node)
	for i := range mounts {
		sm := &mounts[i]
		count, expires := remediationsLastHour(rems.Items, sm, now)
		if count > 0 {
			setRequeue(expires.Sub(now))
		}
		reason, suppressed := dec.Suppressed[sm.Name]
		if suppressed && reason == v1alpha1.SuppressionActiveRemediation && active != nil && covers(active, sm) {
			// Being remediated right now is not a suppression.
			suppressed = false
		}
		prev := meta.FindStatusCondition(sm.Status.Conditions, v1alpha1.ConditionRemediationSuppressed)
		want := TriggerFields{RemediationsLastHour: count, Suppressed: suppressionCondition(prev, reason, suppressed)}
		if suppressed {
			setRequeue(e.suppressedRequeue())
		}
		if triggerFieldsMatch(&sm.Status, want) {
			continue
		}
		newly, err := e.status.ApplyTrigger(ctx, sm.Name, want)
		if err != nil {
			return reconcile.Result{}, err
		}
		if newly {
			log.Info("remediation suppressed", "smbMount", sm.Name, "reason", reason)
			if e.m != nil {
				e.m.RemediationSuppressed.WithLabelValues(string(reason)).Inc()
			}
		}
	}
	if err := e.remirror(ctx, mounts, rems.Items, active); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: requeue}, nil
}

// remirror restores lastRemediation on SmbMounts that lost it (the worker
// deleted and later recreated the object) by re-running the latest
// terminal remediation covering them; Step on a terminal remediation only
// emits its mirror. SmbMounts covered by the active remediation are left
// to it.
func (e *Engine) remirror(ctx context.Context, mounts []v1alpha1.SmbMount, rems []v1alpha1.SmbMountRemediation, active *v1alpha1.SmbMountRemediation) error {
	todo := map[string]bool{}
	for i := range mounts {
		sm := &mounts[i]
		if sm.Status.LastRemediation != nil || (active != nil && covers(active, sm)) {
			continue
		}
		var latest *v1alpha1.SmbMountRemediation
		for j := range rems {
			r := &rems[j]
			if r.Spec.NodeName != sm.Spec.NodeName || !r.Status.Stage.Terminal() || r.DeletionTimestamp != nil || !covers(r, sm) {
				continue
			}
			if latest == nil || remediationFinished(r).After(remediationFinished(latest)) {
				latest = r
			}
		}
		if latest != nil {
			todo[latest.Name] = true
		}
	}
	for name := range todo {
		if _, err := e.ReconcileRemediation(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// suppressedRequeue re-evaluates suppressed findings (backoff expiry, a
// conflicting remediation finishing) without waiting for an SmbMount change.
func (e *Engine) suppressedRequeue() time.Duration {
	d := 10 * e.cfg.PollInterval
	return min(max(d, time.Second), 30*time.Second)
}

// suppressionCondition returns the desired RemediationSuppressed condition:
// True with the reason while suppressed, False once a previously True
// condition clears, absent otherwise.
func suppressionCondition(prev *metav1.Condition, reason v1alpha1.SuppressionReason, suppressed bool) *metav1.Condition {
	if suppressed {
		return &metav1.Condition{
			Type: v1alpha1.ConditionRemediationSuppressed, Status: metav1.ConditionTrue, Reason: string(reason),
			Message: suppressionMessage(reason),
		}
	}
	if prev == nil {
		return nil
	}
	return &metav1.Condition{
		Type: v1alpha1.ConditionRemediationSuppressed, Status: metav1.ConditionFalse, Reason: "NotSuppressed",
		Message: "no remediation is being suppressed",
	}
}

func suppressionMessage(r v1alpha1.SuppressionReason) string {
	switch r {
	case v1alpha1.SuppressionBackoff:
		return "remediation suppressed: backoff after recent remediations of this PV"
	case v1alpha1.SuppressionScopeConflict:
		return "remediation suppressed: an active remediation on another node owns an affected workload"
	case v1alpha1.SuppressionActiveRemediation:
		return "remediation suppressed: another remediation is active on this node"
	}
	return "remediation suppressed"
}

// createRemediation creates obj unless a non-terminal remediation already
// exists for its node in a live (uncached) read, which guards against a
// stale cache creating a second one. It returns a requeue delay.
func (e *Engine) createRemediation(ctx context.Context, obj *v1alpha1.SmbMountRemediation) (time.Duration, error) {
	log := logr.FromContextOrDiscard(ctx)
	node := obj.Spec.NodeName
	var live v1alpha1.SmbMountRemediationList
	if err := e.live.List(ctx, &live, client.MatchingLabels{v1alpha1.LabelNode: node}); err != nil {
		return 0, fmt.Errorf("list remediations of node %s: %w", node, err)
	}
	if a := activeOn(live.Items, node); a != nil {
		log.V(1).Info("not creating remediation: one is already active (cache lag)", "active", a.Name)
		return conflictRetry, nil
	}
	if err := e.c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Same <node>-<unix> name within one second; retry with a new name.
			return time.Second, nil
		}
		return 0, fmt.Errorf("create remediation %s: %w", obj.Name, err)
	}
	log.Info("created remediation", "remediation", obj.Name, "type", obj.Spec.Type,
		"smbMounts", obj.Spec.Trigger.SmbMounts, "reasons", obj.Spec.Trigger.Reasons, "dryRun", obj.Spec.DryRun)
	if e.m != nil {
		e.m.RemediationsStarted.WithLabelValues(node, string(remediationType(obj)), e.mode(obj)).Inc()
	}
	return 0, nil
}

// activeOn returns the oldest non-terminal remediation of node, or nil.
func activeOn(rems []v1alpha1.SmbMountRemediation, node string) *v1alpha1.SmbMountRemediation {
	var out *v1alpha1.SmbMountRemediation
	for i := range rems {
		r := &rems[i]
		if r.Spec.NodeName != node || r.Status.Stage.Terminal() {
			continue
		}
		if out == nil || r.CreationTimestamp.Before(&out.CreationTimestamp) ||
			(r.CreationTimestamp.Equal(&out.CreationTimestamp) && r.Name < out.Name) {
			out = r
		}
	}
	return out
}

// covers reports whether remediation r is about sm (triggered by it or scoped to its PV).
func covers(r *v1alpha1.SmbMountRemediation, sm *v1alpha1.SmbMount) bool {
	for _, n := range r.Spec.Trigger.SmbMounts {
		if n == sm.Name {
			return true
		}
	}
	if r.Status.Scope != nil {
		for _, pv := range r.Status.Scope.PVs {
			if pv == sm.Spec.PVName {
				return true
			}
		}
	}
	return false
}

// remediationsLastHour counts the remediations of sm's node that cover sm
// and started within the last hour, and returns when the oldest of them
// leaves the window.
func remediationsLastHour(rems []v1alpha1.SmbMountRemediation, sm *v1alpha1.SmbMount, now time.Time) (int32, time.Time) {
	var n int32
	var expires time.Time
	for i := range rems {
		r := &rems[i]
		if r.Spec.NodeName != sm.Spec.NodeName || !covers(r, sm) {
			continue
		}
		started := r.CreationTimestamp.Time
		if r.Status.StartedTime != nil {
			started = r.Status.StartedTime.Time
		}
		if started.After(now) || now.Sub(started) >= remediationsWindow {
			continue
		}
		n++
		if exp := started.Add(remediationsWindow); expires.IsZero() || exp.Before(expires) {
			expires = exp
		}
	}
	return n, expires
}

// anyFinding reports whether some SmbMount could make remediation.Trigger
// act (a Broken pod mount or an orphan finding). Without one the node's
// pods are not listed at all.
func anyFinding(mounts []v1alpha1.SmbMount) bool {
	for i := range mounts {
		st := &mounts[i].Status
		for _, pm := range st.PodMounts {
			if pm.Health == v1alpha1.HealthBroken {
				return true
			}
		}
		if g := st.Globalmount; g != nil {
			if g.Mounted && st.Health == v1alpha1.HealthBroken {
				return true
			}
			if !g.Mounted && g.DirExists && !g.Empty {
				return true
			}
		}
		for _, od := range st.OrphanPodDirs {
			if od.Mounted || !od.Empty {
				return true
			}
		}
	}
	return false
}

// healthValues are the smbmounts{health} label values.
var healthValues = []v1alpha1.Health{
	v1alpha1.HealthHealthy, v1alpha1.HealthSuspect, v1alpha1.HealthBroken, v1alpha1.HealthErrored, v1alpha1.HealthUnknown,
}

// RefreshSmbMountGauge sets smbmounts{health} from the cached SmbMounts (an
// empty health counts as Unknown).
func (e *Engine) RefreshSmbMountGauge(ctx context.Context) error {
	if e.m == nil {
		return nil
	}
	var list v1alpha1.SmbMountList
	if err := e.c.List(ctx, &list); err != nil {
		return fmt.Errorf("list SmbMounts: %w", err)
	}
	counts := map[v1alpha1.Health]int{}
	for _, sm := range list.Items {
		h := sm.Status.Health
		if h == "" {
			h = v1alpha1.HealthUnknown
		}
		counts[h]++
	}
	for _, h := range healthValues {
		e.m.SmbMounts.WithLabelValues(string(h)).Set(float64(counts[h]))
	}
	return nil
}
