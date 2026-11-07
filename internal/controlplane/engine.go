package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/internal/remediation"
)

// conflictRetry is the requeue delay after an optimistic-concurrency conflict.
const conflictRetry = 200 * time.Millisecond

// EngineOptions configures NewEngine.
type EngineOptions struct {
	// Client writes everything and reads SmbMounts, SmbMountRemediations,
	// SmbHostTasks and PersistentVolumeClaims. In the manager this is the
	// cache-backed mgr.GetClient(). Required.
	Client client.Client
	// APIReader reads live (uncached): pods by spec.nodeName (field
	// selector), SmbMounts before a status write, and a node's remediations
	// right before creating one. Defaults to Client. A fake client needs a
	// "spec.nodeName" index on Pods.
	APIReader client.Reader
	// Cluster resolves pod owners, reads workloads and lists driver pods. Required.
	Cluster kube.Cluster
	// Executor carries out cluster commands (usually *Executor). Required.
	Executor command.ClusterExecutor
	// Metrics receives the control-plane metrics. Nil disables them.
	Metrics *metrics.ControlPlane
	// Clock is the source of Observation.Now and every timestamp the
	// control plane writes. Defaults to clock.RealClock{}.
	Clock clock.PassiveClock
	// Config holds the timings, dry run, retention and backoff settings.
	Config config.ControlPlane
}

// Engine is the control plane's logic, independent of controller-runtime's
// manager: each Reconcile* method reads the world, decides through the pure
// remediation package and writes the result. The controllers in
// controllers.go call it; internal/sim can drive it directly with any
// client and clock.
type Engine struct {
	c      client.Client
	live   client.Reader
	kc     kube.Cluster
	exec   command.ClusterExecutor
	status *SmbMountStatusWriter
	m      *metrics.ControlPlane
	clock  clock.PassiveClock
	cfg    config.ControlPlane
	rcfg   remediation.Config
	owners *ownerMemo

	mu sync.Mutex
	// seen are the remediations reconciled since process start (for
	// remediations_resumed_total).
	seen map[string]bool
	// gauges are the remediation_stage base labels per remediation.
	gauges map[string]prometheus.Labels
}

// NewEngine returns an Engine.
func NewEngine(o EngineOptions) (*Engine, error) {
	if o.Client == nil || o.Cluster == nil || o.Executor == nil {
		return nil, errors.New("controlplane: EngineOptions.Client, .Cluster and .Executor are required")
	}
	if o.APIReader == nil {
		o.APIReader = o.Client
	}
	if o.Clock == nil {
		o.Clock = clock.RealClock{}
	}
	return &Engine{
		c: o.Client, live: o.APIReader, kc: o.Cluster, exec: o.Executor, m: o.Metrics, clock: o.Clock,
		status: NewSmbMountStatusWriter(o.APIReader, o.Executor, o.Clock),
		cfg:    o.Config, rcfg: RemediationConfig(o.Config), owners: newOwnerMemo(),
		seen: map[string]bool{}, gauges: map[string]prometheus.Labels{},
	}, nil
}

// StatusWriter returns the writer all control-plane SmbMount status writes go through.
func (e *Engine) StatusWriter() *SmbMountStatusWriter { return e.status }

// RemediationConfig maps the control-plane configuration to remediation.Config.
func RemediationConfig(cp config.ControlPlane) remediation.Config {
	return remediation.Config{
		DryRun:                cp.DryRun,
		PollInterval:          cp.PollInterval,
		PlanningProbeWait:     cp.PlanningProbeWait,
		ScaleDownWait:         cp.ScaleDownWait,
		UnstageWait:           cp.UnstageWait,
		QuarantineUnstageWait: cp.QuarantineUnstageWait,
		DriverRestartWait:     cp.DriverRestartWait,
		VerifyWait:            cp.VerifyWait,
		VerifyNoPodGrace:      cp.VerifyNoPodGrace,
		EpisodeMaxDuration:    cp.EpisodeMaxDuration,
		BackoffBase:           cp.BackoffBase,
		BackoffMax:            cp.BackoffMax,
	}
}

// ReconcileRemediation advances one SmbMountRemediation:
//
//  1. ensure the restore-replicas finalizer on a non-terminal object;
//  2. build the Observation and run remediation.Step;
//  3. write the new status first (write-ahead; a conflict requeues without
//     executing anything);
//  4. execute the commands (SmbMount mirror patches go through the
//     SmbMountStatusWriter);
//  5. remove the finalizer once the stage is done.
func (e *Engine) ReconcileRemediation(ctx context.Context, name string) (reconcile.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("remediation", name)
	ctx = logr.NewContext(ctx, log)

	var r v1alpha1.SmbMountRemediation
	if err := e.c.Get(ctx, client.ObjectKey{Name: name}, &r); err != nil {
		if apierrors.IsNotFound(err) {
			e.forget(name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	e.noteSeen(&r)

	if !r.Status.Stage.Terminal() && r.DeletionTimestamp == nil && !controllerutil.ContainsFinalizer(&r, v1alpha1.FinalizerRestoreReplicas) {
		patch := client.MergeFromWithOptions(r.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(&r, v1alpha1.FinalizerRestoreReplicas)
		if err := e.c.Patch(ctx, &r, patch); err != nil {
			return requeueOnConflict(err)
		}
	}

	obs, err := e.observe(ctx, &r)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("observe: %w", err)
	}
	res := remediation.Step(&r, obs, e.rcfg)

	prev := r.Status.DeepCopy()
	changed, err := statusChanged(&r.Status, &res.Status)
	if err != nil {
		return reconcile.Result{}, err
	}
	if changed {
		upd := r.DeepCopy()
		upd.Status = res.Status
		if err := e.c.Status().Update(ctx, upd); err != nil {
			return requeueOnConflict(err)
		}
		r = *upd
		for _, t := range res.Transitions {
			log.Info("stage transition", "from", t.From, "to", t.To, "reason", t.Reason)
		}
	}
	e.recordStep(&r, prev, res)

	var errs []error
	for _, cmd := range res.Commands {
		if err := e.execute(ctx, cmd); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return reconcile.Result{}, errors.Join(errs...)
	}

	if r.Status.Stage.Terminal() && controllerutil.ContainsFinalizer(&r, v1alpha1.FinalizerRestoreReplicas) {
		patch := client.MergeFromWithOptions(r.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.RemoveFinalizer(&r, v1alpha1.FinalizerRestoreReplicas)
		if err := e.c.Patch(ctx, &r, patch); err != nil && !apierrors.IsNotFound(err) {
			return requeueOnConflict(err)
		}
	}
	return reconcile.Result{RequeueAfter: res.RequeueAfter}, nil
}

func (e *Engine) execute(ctx context.Context, cmd command.ClusterCommand) error {
	if m, ok := cmd.(command.PatchSmbMountRemediationMirror); ok {
		return e.status.ApplyMirror(ctx, m)
	}
	return e.exec.ExecuteCluster(ctx, cmd)
}

func requeueOnConflict(err error) (reconcile.Result, error) {
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return reconcile.Result{RequeueAfter: conflictRetry}, nil
	}
	return reconcile.Result{}, err
}

// statusChanged compares the JSON forms, so nil and empty lists (both
// omitted) are equal and a no-op Step never writes.
func statusChanged(a, b *v1alpha1.SmbMountRemediationStatus) (bool, error) {
	ja, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(ja, jb), nil
}

func (e *Engine) mode(r *v1alpha1.SmbMountRemediation) string {
	if r.Spec.DryRun || e.cfg.DryRun {
		return metrics.ModeDryRun
	}
	return metrics.ModeLive
}

func remediationType(r *v1alpha1.SmbMountRemediation) v1alpha1.RemediationType {
	if r.Spec.Type == "" {
		return v1alpha1.RemediationTypeWorkload
	}
	return r.Spec.Type
}

// noteSeen counts remediations_resumed_total for a non-terminal remediation
// with a persisted stage that this process sees for the first time.
func (e *Engine) noteSeen(r *v1alpha1.SmbMountRemediation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seen[r.Name] {
		return
	}
	e.seen[r.Name] = true
	if r.Status.Stage == "" || r.Status.Stage.Terminal() {
		return
	}
	if e.m != nil {
		e.m.RemediationsResumed.WithLabelValues(string(r.Status.Stage)).Inc()
	}
}

func (e *Engine) forget(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.seen, name)
	if base, ok := e.gauges[name]; ok {
		if e.m != nil {
			metrics.DeleteEnum(e.m.RemediationStage, base)
		}
		delete(e.gauges, name)
	}
}

// stageValues are the remediation_stage enum values per machine: every
// non-terminal stage of its transition table.
var stageValues = func() map[v1alpha1.RemediationType][]string {
	out := map[v1alpha1.RemediationType][]string{}
	seen := map[v1alpha1.RemediationType]map[v1alpha1.Stage]bool{}
	add := func(t v1alpha1.RemediationType, s v1alpha1.Stage) {
		if s == "" || s.Terminal() {
			return
		}
		if seen[t] == nil {
			seen[t] = map[v1alpha1.Stage]bool{}
		}
		if !seen[t][s] {
			seen[t][s] = true
			out[t] = append(out[t], string(s))
		}
	}
	for _, r := range remediation.Rules() {
		for _, f := range r.From {
			add(r.Machine, f)
		}
		add(r.Machine, r.To)
	}
	return out
}()

// recordStep records the metrics of one persisted Step result. prev is the
// status before the Step.
func (e *Engine) recordStep(r *v1alpha1.SmbMountRemediation, prev *v1alpha1.SmbMountRemediationStatus, res remediation.Result) {
	if e.m == nil {
		return
	}
	typ := remediationType(r)
	entered := prev.StageEnteredTime
	for _, t := range res.Transitions {
		e.m.RemediationTransitions.WithLabelValues(string(typ), string(t.From), string(t.To), t.Reason).Inc()
		if t.From != "" && entered != nil {
			e.m.RemediationStageDuration.WithLabelValues(string(typ), string(t.From), t.Reason).
				Observe(nonNegative(t.Time.Sub(entered.Time)).Seconds())
		}
		entered = &metav1.Time{Time: t.Time}
		if t.To.Terminal() {
			out := string(res.Status.Outcome)
			e.m.Remediations.WithLabelValues(r.Spec.NodeName, string(typ), e.mode(r), out).Inc()
			if st := res.Status.StartedTime; st != nil {
				e.m.RemediationDuration.WithLabelValues(string(typ), out).Observe(nonNegative(t.Time.Sub(st.Time)).Seconds())
			}
			for _, v := range res.Status.Verification {
				e.m.WorkloadVerifications.WithLabelValues(string(v.Result)).Inc()
			}
		}
	}
	base := prometheus.Labels{metrics.LabelTargetNode: r.Spec.NodeName, metrics.LabelType: string(typ)}
	e.mu.Lock()
	defer e.mu.Unlock()
	if res.Status.Stage.Terminal() || res.Status.Stage == "" {
		if _, ok := e.gauges[r.Name]; ok {
			metrics.DeleteEnum(e.m.RemediationStage, base)
			delete(e.gauges, r.Name)
		}
		return
	}
	e.gauges[r.Name] = base
	metrics.SetEnum(e.m.RemediationStage, base, metrics.LabelStage, stageValues[typ], string(res.Status.Stage))
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
