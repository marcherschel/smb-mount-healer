package controlplane

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// gcMaxRequeue bounds the GC requeue so a retention change is picked up.
const gcMaxRequeue = time.Hour

// ReconcileGC prunes the terminal remediations of node that finished more
// than REMEDIATION_RETENTION ago, always keeping the newest
// REMEDIATION_RETAIN_MIN_PER_NODE. Their SmbHostTasks go with them through
// ownerReferences. Non-terminal remediations are never touched (their
// rollback is the finalizer's job, see ReconcileRemediation).
func (e *Engine) ReconcileGC(ctx context.Context, node string) (reconcile.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("node", node)
	var list v1alpha1.SmbMountRemediationList
	if err := e.c.List(ctx, &list); err != nil {
		return reconcile.Result{}, fmt.Errorf("list remediations: %w", err)
	}
	var mine []v1alpha1.SmbMountRemediation
	for _, r := range list.Items {
		if r.Spec.NodeName == node {
			mine = append(mine, r)
		}
	}
	now := e.clock.Now()
	del, next := selectGC(mine, now, e.cfg.RemediationRetention, e.cfg.RemediationRetainMinPerNode)
	for i := range del {
		r := &del[i]
		uid := r.UID
		if err := e.c.Delete(ctx, r, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return reconcile.Result{}, fmt.Errorf("delete remediation %s: %w", r.Name, err)
		}
		log.Info("deleted expired remediation", "remediation", r.Name, "outcome", r.Status.Outcome)
	}
	if next <= 0 || next > gcMaxRequeue {
		next = gcMaxRequeue
	}
	return reconcile.Result{RequeueAfter: next}, nil
}

// remediationFinished is when a terminal remediation reached done.
func remediationFinished(r *v1alpha1.SmbMountRemediation) time.Time {
	if r.Status.StageEnteredTime != nil {
		return r.Status.StageEnteredTime.Time
	}
	return r.CreationTimestamp.Time
}

// selectGC returns the remediations to delete and the delay until the next
// one expires (0: none pending). Only terminal objects not already being
// deleted count; the newest keepMin of them are always kept.
func selectGC(rems []v1alpha1.SmbMountRemediation, now time.Time, retention time.Duration, keepMin int) ([]v1alpha1.SmbMountRemediation, time.Duration) {
	var terminal []v1alpha1.SmbMountRemediation
	for _, r := range rems {
		if r.Status.Stage.Terminal() && r.DeletionTimestamp == nil {
			terminal = append(terminal, r)
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		a, b := remediationFinished(&terminal[i]), remediationFinished(&terminal[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return terminal[i].Name > terminal[j].Name
	})
	var del []v1alpha1.SmbMountRemediation
	var next time.Duration
	for i := range terminal {
		if i < keepMin {
			continue
		}
		exp := remediationFinished(&terminal[i]).Add(retention)
		if !now.Before(exp) {
			del = append(del, terminal[i])
			continue
		}
		if d := exp.Sub(now); next == 0 || d < next {
			next = d
		}
	}
	return del, next
}
