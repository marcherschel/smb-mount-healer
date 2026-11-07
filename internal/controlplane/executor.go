package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	ac "github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// ExecutorOptions configures NewExecutor.
type ExecutorOptions struct {
	// Cluster performs the workload and driver-pod actions and Events. Required.
	Cluster kube.Cluster
	// Client creates/deletes SmbHostTasks and server-side-applies SmbMount
	// status. Its scheme must contain v1alpha1 and the built-in kinds. Required.
	Client client.Client
	// Metrics receives workload_actions_total. Nil disables metrics.
	Metrics *metrics.ControlPlane
	// DryRun makes the executor refuse ScaleWorkload and DeletePod (logged
	// and counted with mode=dry_run, never sent) and force spec.dryRun on
	// every SmbHostTask it creates.
	DryRun bool
	// Clock stamps conditions whose LastTransitionTime is unset. Defaults to clock.RealClock{}.
	Clock clock.PassiveClock
}

// Executor is the control plane's command.ClusterExecutor.
//
// Every command is idempotent; "already done" outcomes are success:
//   - ScaleWorkload: no-op if replicas already match; a vanished workload is
//     logged and skipped (the FSM notices it through kube.Cluster.Workload).
//   - DeletePod: NotFound or a UID-precondition conflict (see kube.Cluster).
//   - CreateHostTask: AlreadyExists.
//   - DeleteHostTask: NotFound.
//   - PatchSmbMountRemediationMirror: NotFound (the worker removed the SmbMount).
type Executor struct {
	cluster kube.Cluster
	c       client.Client
	m       *metrics.ControlPlane
	dryRun  bool
	clock   clock.PassiveClock
}

var _ command.ClusterExecutor = (*Executor)(nil)

// NewExecutor returns a cluster executor.
func NewExecutor(o ExecutorOptions) (*Executor, error) {
	if o.Cluster == nil || o.Client == nil {
		return nil, errors.New("controlplane: ExecutorOptions.Cluster and .Client are required")
	}
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	return &Executor{cluster: o.Cluster, c: o.Client, m: o.Metrics, dryRun: o.DryRun, clock: o.Clock}, nil
}

// ExecuteCluster implements command.ClusterExecutor.
func (e *Executor) ExecuteCluster(ctx context.Context, cmd command.ClusterCommand) error {
	switch c := cmd.(type) {
	case command.ScaleWorkload:
		return e.scale(ctx, c)
	case command.DeletePod:
		return e.deletePod(ctx, c)
	case command.CreateHostTask:
		return e.createHostTask(ctx, c)
	case command.DeleteHostTask:
		return e.deleteHostTask(ctx, c)
	case command.EmitEvent:
		return e.emitEvent(c)
	case command.PatchSmbMountRemediationMirror:
		return e.patchMirror(ctx, c)
	default:
		return fmt.Errorf("controlplane: unsupported cluster command %T", cmd)
	}
}

func (e *Executor) mode() string {
	if e.dryRun {
		return metrics.ModeDryRun
	}
	return metrics.ModeLive
}

func (e *Executor) countAction(action command.WorkloadAction, result string) {
	if e.m == nil {
		return
	}
	e.m.WorkloadActions.With(prometheus.Labels{
		metrics.LabelAction: string(action), metrics.LabelResult: result, metrics.LabelMode: e.mode(),
	}).Inc()
}

func (e *Executor) scale(ctx context.Context, c command.ScaleWorkload) error {
	log := logr.FromContextOrDiscard(ctx).WithValues("workload", c.Workload.String(), "replicas", c.Replicas, "action", c.Action)
	if e.dryRun {
		log.Info("dry run: not scaling workload")
		e.countAction(c.Action, metrics.ResultSuccess)
		return nil
	}
	changed := true
	var err error
	if r, ok := e.cluster.(kube.ChangeReporter); ok {
		changed, err = r.ScaleReport(ctx, c.Workload, c.Replicas)
	} else {
		err = e.cluster.Scale(ctx, c.Workload, c.Replicas)
	}
	switch {
	case apierrors.IsNotFound(err):
		log.Info("workload not found, nothing to scale")
		return nil
	case err != nil:
		e.countAction(c.Action, metrics.ResultError)
		return err
	case changed:
		log.Info("scaled workload")
		e.countAction(c.Action, metrics.ResultSuccess)
	}
	return nil
}

func (e *Executor) deletePod(ctx context.Context, c command.DeletePod) error {
	log := logr.FromContextOrDiscard(ctx).WithValues("pod", c.Namespace+"/"+c.Name, "uid", c.UID, "action", c.Action)
	if e.dryRun {
		log.Info("dry run: not deleting pod")
		e.countAction(c.Action, metrics.ResultSuccess)
		return nil
	}
	deleted := true
	var err error
	if r, ok := e.cluster.(kube.ChangeReporter); ok {
		deleted, err = r.DeletePodReport(ctx, c.Namespace, c.Name, c.UID)
	} else {
		err = e.cluster.DeletePod(ctx, c.Namespace, c.Name, c.UID)
	}
	if err != nil {
		e.countAction(c.Action, metrics.ResultError)
		return err
	}
	if deleted {
		log.Info("deleted pod")
		e.countAction(c.Action, metrics.ResultSuccess)
	}
	return nil
}

func (e *Executor) createHostTask(ctx context.Context, c command.CreateHostTask) error {
	if c.Owner.Name == "" || c.Owner.UID == "" {
		return fmt.Errorf("create host task %s: owner name and UID are required", c.Name)
	}
	dryRun := c.DryRun
	if e.dryRun && !dryRun {
		logr.FromContextOrDiscard(ctx).Info("dry run: forcing spec.dryRun on host task", "task", c.Name)
		dryRun = true
	}
	task := &v1alpha1.SmbHostTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:   c.Name,
			Labels: map[string]string{v1alpha1.LabelNode: c.NodeName, v1alpha1.LabelRemediation: c.Owner.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.GroupVersion.String(),
				Kind:               "SmbMountRemediation",
				Name:               c.Owner.Name,
				UID:                c.Owner.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: v1alpha1.SmbHostTaskSpec{NodeName: c.NodeName, DryRun: dryRun, Actions: c.Actions},
	}
	if err := e.c.Create(ctx, task); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create host task %s: %w", c.Name, err)
	}
	return nil
}

func (e *Executor) deleteHostTask(ctx context.Context, c command.DeleteHostTask) error {
	task := &v1alpha1.SmbHostTask{ObjectMeta: metav1.ObjectMeta{Name: c.Name}}
	if err := e.c.Delete(ctx, task, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete host task %s: %w", c.Name, err)
	}
	return nil
}

// eventKinds maps command.ObjectRef.Kind to its API group/version.
var eventKinds = map[string]schema.GroupVersion{
	"SmbMountRemediation": v1alpha1.GroupVersion,
	"SmbMount":            v1alpha1.GroupVersion,
	"SmbHostTask":         v1alpha1.GroupVersion,
	"SmbQuarantine":       v1alpha1.GroupVersion,
	"Pod":                 {Version: "v1"},
	"Node":                {Version: "v1"},
	"Deployment":          {Group: "apps", Version: "v1"},
	"StatefulSet":         {Group: "apps", Version: "v1"},
}

func (e *Executor) emitEvent(c command.EmitEvent) error {
	gv, ok := eventKinds[c.Regarding.Kind]
	if !ok {
		return fmt.Errorf("emit event %s: unsupported regarding kind %q", c.Reason, c.Regarding.Kind)
	}
	gvk := gv.WithKind(c.Regarding.Kind)
	obj, err := e.c.Scheme().New(gvk)
	if err != nil {
		return fmt.Errorf("emit event %s: %w", c.Reason, err)
	}
	acc, err := meta.Accessor(obj)
	if err != nil {
		return fmt.Errorf("emit event %s: %w", c.Reason, err)
	}
	acc.SetNamespace(c.Regarding.Namespace)
	acc.SetName(c.Regarding.Name)
	acc.SetUID(c.Regarding.UID)
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	e.cluster.Event(obj, c.Type, c.Reason, c.Action, c.Message)
	return nil
}

// controlPlaneConditions are the SmbMount condition types the control plane owns.
var controlPlaneConditions = map[string]bool{
	v1alpha1.ConditionRemediating:           true,
	v1alpha1.ConditionDegraded:              true,
	v1alpha1.ConditionRemediationSuppressed: true,
}

// patchMirror server-side-applies only the control plane's SmbMount status
// fields under FieldManagerControlPlane. Fields left nil in the command are
// omitted, which releases (and so removes) them.
func (e *Executor) patchMirror(ctx context.Context, c command.PatchSmbMountRemediationMirror) error {
	st := ac.SmbMountStatus()
	if r := c.Remediation; r != nil {
		a := ac.ActiveRemediation().WithActive(r.Active)
		if r.Name != "" {
			a.WithName(r.Name)
		}
		if r.Stage != "" {
			a.WithStage(r.Stage)
		}
		if r.StageEnteredTime != nil {
			a.WithStageEnteredTime(*r.StageEnteredTime)
		}
		st.WithRemediation(a)
	}
	if l := c.LastRemediation; l != nil {
		a := ac.LastRemediation().WithName(l.Name)
		if l.Outcome != "" {
			a.WithOutcome(l.Outcome)
		}
		if l.StartedTime != nil {
			a.WithStartedTime(*l.StartedTime)
		}
		if l.FinishedTime != nil {
			a.WithFinishedTime(*l.FinishedTime)
		}
		st.WithLastRemediation(a)
	}
	if c.RemediationsLastHour != nil {
		st.WithRemediationsLastHour(*c.RemediationsLastHour)
	}
	conds, err := e.mirrorConditions(ctx, c)
	if err != nil {
		return err
	}
	st.WithConditions(conds...)

	err = e.c.Status().Apply(ctx, ac.SmbMount(c.SmbMount).WithStatus(st),
		client.FieldOwner(v1alpha1.FieldManagerControlPlane), client.ForceOwnership)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("apply remediation mirror on SmbMount %s: %w", c.SmbMount, err)
	}
	return nil
}

// mirrorConditions validates the condition types and fills unset
// LastTransitionTimes: kept from the live condition if its status is
// unchanged, else now.
func (e *Executor) mirrorConditions(ctx context.Context, c command.PatchSmbMountRemediationMirror) ([]*metav1ac.ConditionApplyConfiguration, error) {
	var live []metav1.Condition
	needLive := false
	for _, cnd := range c.Conditions {
		if !controlPlaneConditions[cnd.Type] {
			return nil, fmt.Errorf("apply remediation mirror on SmbMount %s: condition %q is not owned by the control plane", c.SmbMount, cnd.Type)
		}
		needLive = needLive || cnd.LastTransitionTime.IsZero()
	}
	if needLive {
		var sm v1alpha1.SmbMount
		err := e.c.Get(ctx, client.ObjectKey{Name: c.SmbMount}, &sm)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get SmbMount %s: %w", c.SmbMount, err)
		}
		live = sm.Status.Conditions
	}
	out := make([]*metav1ac.ConditionApplyConfiguration, 0, len(c.Conditions))
	for _, cnd := range c.Conditions {
		ltt := cnd.LastTransitionTime
		if ltt.IsZero() {
			ltt = metav1.NewTime(e.clock.Now())
			if old := meta.FindStatusCondition(live, cnd.Type); old != nil && old.Status == cnd.Status && !old.LastTransitionTime.IsZero() {
				ltt = old.LastTransitionTime
			}
		}
		a := metav1ac.Condition().WithType(cnd.Type).WithStatus(cnd.Status).WithReason(cnd.Reason).
			WithMessage(cnd.Message).WithLastTransitionTime(ltt)
		if cnd.ObservedGeneration != 0 {
			a.WithObservedGeneration(cnd.ObservedGeneration)
		}
		out = append(out, a)
	}
	return out, nil
}
