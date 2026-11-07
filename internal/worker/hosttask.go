package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// Label values of host_actions_total.
const (
	ActionLabelForceUnmount = "force_unmount"
	ActionLabelQuarantine   = "quarantine"
	ActionLabelProbeNow     = "probe_now"
	ActionLabelUnknown      = "unknown"

	ResultLabelSucceeded = "succeeded"
	ResultLabelSkipped   = "skipped"
	ResultLabelDryRun    = "dry_run"
	ResultLabelFailed    = "failed"
)

// HostTaskReconciler runs the actions of this node's SmbHostTasks through
// the HostExecutor and records their results. Actions whose id already has
// a result are never run again, and results are persisted after every
// action, so a worker that restarts halfway through a task finishes it
// safely. The task is read live (not from the cache) so a stale cache can
// never cause a re-run. A dry-run task only logs what it would do (the
// executor returns DryRun results). When the task contained a ProbeNow or
// changed the host (a successful ForceUnmount or Quarantine), the final
// status is written only after a prober round that started afterwards has
// been published (bounded by 2*STAT_TIMEOUT+5s), so the control plane sees
// fresh health and the new mount/directory state once the task is done.
type HostTaskReconciler struct {
	w *Worker
}

func (r *HostTaskReconciler) setup(mgr manager.Manager) error {
	node := r.w.node
	return ctrl.NewControllerManagedBy(mgr).
		Named("smbhosttask").
		For(&v1alpha1.SmbHostTask{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			t, ok := o.(*v1alpha1.SmbHostTask)
			return ok && t.Spec.NodeName == node
		}))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1, SkipNameValidation: ptr.To(true)}).
		Complete(r)
}

// probeNowTimeout bounds how long a task waits for the prober round that
// reflects it to be published.
func (r *HostTaskReconciler) probeNowTimeout() time.Duration {
	return 2*r.w.cfg.StatTimeout + 5*time.Second
}

// Reconcile implements reconcile.Reconciler.
func (r *HostTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var task v1alpha1.SmbHostTask
	if err := r.w.api.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if task.Spec.NodeName != r.w.node || !task.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	done := map[string]bool{}
	// planned: write-ahead records of Quarantine actions interrupted by a
	// crash (InProgress); the action is re-run and keeps that destination.
	planned := map[string]string{}
	failed := false
	for _, res := range task.Status.Results {
		if res.Result == v1alpha1.HostActionInProgress {
			planned[res.ID] = res.Destination
			continue
		}
		done[res.ID] = true
		failed = failed || res.Result == v1alpha1.HostActionFailed
	}
	pending := 0
	for _, a := range task.Spec.Actions {
		if !done[a.ID] {
			pending++
		}
	}
	final := v1alpha1.HostTaskSucceeded
	if failed {
		final = v1alpha1.HostTaskFailed
	}
	if pending == 0 && task.Status.Phase == final && task.Status.ObservedGeneration == task.Generation {
		return ctrl.Result{}, nil
	}

	log := r.w.logger(ctx).WithValues("smbHostTask", task.Name, "dryRun", task.Spec.DryRun)
	ctx = logr.NewContext(ctx, log)
	if pending > 0 {
		log.Info("running host task", "pendingActions", pending, "actions", len(task.Spec.Actions))
	}
	var probeSeq uint64
	for _, a := range task.Spec.Actions {
		if done[a.ID] {
			continue
		}
		var res v1alpha1.HostActionResult
		cmd, err := command.FromHostAction(a)
		if err != nil {
			res = v1alpha1.HostActionResult{ID: a.ID, Result: v1alpha1.HostActionFailed, Error: err.Error(),
				Time: metav1.NewTime(r.w.clock.Now())}
		} else {
			if a.Type == v1alpha1.HostActionProbeNow {
				probeSeq = r.w.Prober.nextRoundSeq()
			}
			ex := r.executor(&task, a)
			if a.Type == v1alpha1.HostActionQuarantine && !task.Spec.DryRun {
				resumed := planned[a.ID] != ""
				if !resumed {
					planned[a.ID] = filepath.Join(r.w.exec.QuarantineDir, kubelet.QuarantineFolderName(a.PV, r.w.clock.Now()))
					setResult(&task, v1alpha1.HostActionResult{ID: a.ID, Result: v1alpha1.HostActionInProgress,
						Destination: planned[a.ID], Time: metav1.NewTime(r.w.clock.Now())})
					task.Status.Phase = v1alpha1.HostTaskRunning
					task.Status.ObservedGeneration = task.Generation
					if err := r.w.c.Status().Update(ctx, &task); err != nil {
						return ctrl.Result{}, fmt.Errorf("record planned quarantine of action %s: %w", a.ID, err)
					}
				}
				ex.PlannedDestination = planned[a.ID]
				res = ex.ExecuteHost(ctx, task.Spec.DryRun, cmd)
				if resumed && res.Destination == "" && res.Result == v1alpha1.HostActionSucceeded {
					// The interrupted run already moved the data: keep its
					// destination so the provenance stays attached.
					res.Destination = planned[a.ID]
				}
			} else {
				res = ex.ExecuteHost(ctx, task.Spec.DryRun, cmd)
			}
			if a.Type != v1alpha1.HostActionProbeNow && res.Result == v1alpha1.HostActionSucceeded {
				// The host changed: refresh the inventory right away.
				probeSeq = r.w.Prober.nextRoundSeq()
				r.w.Prober.Refresh()
			}
		}
		r.w.m.HostActions.WithLabelValues(actionLabel(a.Type), resultLabel(res.Result), strconv.FormatBool(task.Spec.DryRun)).Inc()
		done[a.ID] = true
		pending--
		failed = failed || res.Result == v1alpha1.HostActionFailed
		setResult(&task, res)
		task.Status.Phase = v1alpha1.HostTaskRunning
		task.Status.ObservedGeneration = task.Generation
		if pending > 0 {
			if err := r.w.c.Status().Update(ctx, &task); err != nil {
				return ctrl.Result{}, fmt.Errorf("record result of action %s: %w", a.ID, err)
			}
		}
	}
	if probeSeq > 0 && !r.w.manual && !r.w.Publisher.waitPublished(ctx, probeSeq, r.probeNowTimeout()) {
		log.Info("prober round reflecting the task not published in time; completing the task anyway", "timeout", r.probeNowTimeout())
	}
	task.Status.Phase = v1alpha1.HostTaskSucceeded
	if failed {
		task.Status.Phase = v1alpha1.HostTaskFailed
	}
	task.Status.ObservedGeneration = task.Generation
	if err := r.w.c.Status().Update(ctx, &task); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("complete host task: %w", err)
	}
	log.Info("host task finished", "phase", task.Status.Phase)
	return ctrl.Result{}, nil
}

// executor returns the shared executor with an OnQuarantined hook that
// records where the data came from before triggering a quarantine scan.
func (r *HostTaskReconciler) executor(task *v1alpha1.SmbHostTask, a v1alpha1.HostAction) *HostExecutor {
	ex := *r.w.exec
	p := provenance{sourcePath: a.Path, remediation: taskRemediation(task)}
	ex.OnQuarantined = func(dest string) {
		r.w.Scanner.recordProvenance(dest, p)
		r.w.Scanner.Trigger()
	}
	return &ex
}

// setResult replaces the result with res.ID (an InProgress write-ahead
// record) or appends it.
func setResult(task *v1alpha1.SmbHostTask, res v1alpha1.HostActionResult) {
	for i := range task.Status.Results {
		if task.Status.Results[i].ID == res.ID {
			task.Status.Results[i] = res
			return
		}
	}
	task.Status.Results = append(task.Status.Results, res)
}

func actionLabel(t v1alpha1.HostActionType) string {
	switch t {
	case v1alpha1.HostActionForceUnmount:
		return ActionLabelForceUnmount
	case v1alpha1.HostActionQuarantine:
		return ActionLabelQuarantine
	case v1alpha1.HostActionProbeNow:
		return ActionLabelProbeNow
	}
	return ActionLabelUnknown
}

func resultLabel(o v1alpha1.HostActionOutcome) string {
	switch o {
	case v1alpha1.HostActionSucceeded:
		return ResultLabelSucceeded
	case v1alpha1.HostActionSkipped:
		return ResultLabelSkipped
	case v1alpha1.HostActionDryRun:
		return ResultLabelDryRun
	case v1alpha1.HostActionFailed, v1alpha1.HostActionInProgress:
		// InProgress is a write-ahead record, never a counted final result.
		return ResultLabelFailed
	}
	return ResultLabelFailed
}

// deleteHandler calls fn on deletes only.
func deleteHandler(fn func()) toolscache.ResourceEventHandlerFuncs {
	return toolscache.ResourceEventHandlerFuncs{DeleteFunc: func(any) { fn() }}
}
