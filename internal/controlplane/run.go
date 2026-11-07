package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// EventReporter is the events.k8s.io reportingController of the control plane.
const EventReporter = "smb-mount-healer-controlplane"

// Options are dependencies injectable by tests and internal/sim. The zero
// value is the production setup.
type Options struct {
	Clock    clock.Clock // defaults to clock.RealClock{}
	Version  string
	Revision string

	// RESTConfig overrides ctrl.GetConfig().
	RESTConfig *rest.Config
	// Registry receives the control-plane collectors and build_info.
	// Defaults to controller-runtime's registry (served on the metrics endpoint).
	Registry prometheus.Registerer
	// Metrics are the collectors to use; they are registered on Registry.
	// Defaults to metrics.NewControlPlane().
	Metrics *metrics.ControlPlane
	// WrapClient wraps the manager's client before the control plane uses
	// it for anything (tests: interceptors).
	WrapClient func(client.Client) client.Client
	// LeaseDuration, RenewDeadline and RetryPeriod override the leader
	// election timings (zero: controller-runtime defaults 15s/10s/2s).
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	// KeepLeaseOnCancel disables releasing the Lease on a graceful stop, so
	// a successor has to wait for it to expire (tests of a crashed leader).
	KeepLeaseOnCancel bool
	// SkipControllerNameValidation allows several managers in one process (tests).
	SkipControllerNameValidation bool
}

// Run starts the control plane and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, opts Options) error {
	logr.FromContextOrDiscard(ctx).Info("starting control plane", "dryRun", cfg.ControlPlane.DryRun,
		"leaderElection", cfg.ControlPlane.LeaderElection, "version", opts.Version, "revision", opts.Revision)
	mgr, _, err := NewManager(cfg, opts)
	if err != nil {
		return err
	}
	return mgr.Start(ctx)
}

// NewManager builds the leader-elected manager with the control plane's
// controllers, health checks and metrics, without starting it.
//
// Cache scoping (the control plane must not hold every pod of the cluster):
//
//   - Pods: the informer only covers CSI_NODE_DRIVER_NAMESPACE with
//     CSI_NODE_DRIVER_LABEL_SELECTOR (driver pods, watched for restarts).
//     Pods on a remediation's node are listed live with the field selector
//     spec.nodeName (an indexed list in the API server, the same one kubelet
//     uses), only while a remediation is active or the node has a finding;
//     resolved owners are memoized per pod UID. Reading a pod outside the
//     driver scope through the cached client fails instead of silently
//     starting a cluster-wide informer.
//   - PersistentVolumeClaims: cluster-wide, transformed down to name,
//     namespace, UID and spec.volumeName (pod -> PV resolution).
//   - Deployments/StatefulSets: metadata-only informers (events only);
//     their replicas are read live through the scale-aware kube.Cluster.
//     ReplicaSets are only ever read live (owner resolution). Typed reads of
//     all three bypass the cache.
//   - SmbMounts, SmbMountRemediations, SmbHostTasks: fully cached.
func NewManager(cfg config.Config, opts Options) (manager.Manager, *Engine, error) {
	cp := cfg.ControlPlane
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	if opts.Registry == nil {
		opts.Registry = ctrlmetrics.Registry
	}
	restCfg := opts.RESTConfig
	if restCfg == nil {
		var err error
		if restCfg, err = ctrl.GetConfig(); err != nil {
			return nil, nil, fmt.Errorf("kubeconfig: %w", err)
		}
	}
	driverSel, err := labels.Parse(cp.CSINodeDriverLabelSelector)
	if err != nil {
		return nil, nil, fmt.Errorf("CSI_NODE_DRIVER_LABEL_SELECTOR: %w", err)
	}
	if driverSel.Empty() {
		return nil, nil, errors.New("CSI_NODE_DRIVER_LABEL_SELECTOR must not be empty")
	}
	scheme, err := kube.NewScheme()
	if err != nil {
		return nil, nil, err
	}

	mo := ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {Namespaces: map[string]cache.Config{
				cp.CSINodeDriverNamespace: {LabelSelector: driverSel},
			}},
			&corev1.PersistentVolumeClaim{}: {Transform: stripPVC},
		}},
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&appsv1.Deployment{}, &appsv1.StatefulSet{}, &appsv1.ReplicaSet{}},
		}},
		Metrics:                       metricsserver.Options{BindAddress: cfg.Common.MetricsBindAddress},
		HealthProbeBindAddress:        cfg.Common.HealthProbeBindAddress,
		LeaderElection:                cp.LeaderElection,
		LeaderElectionID:              cp.LeaderElectionID,
		LeaderElectionNamespace:       cp.LeaderElectionNamespace,
		LeaderElectionReleaseOnCancel: !opts.KeepLeaseOnCancel,
	}
	if opts.LeaseDuration > 0 {
		mo.LeaseDuration = &opts.LeaseDuration
	}
	if opts.RenewDeadline > 0 {
		mo.RenewDeadline = &opts.RenewDeadline
	}
	if opts.RetryPeriod > 0 {
		mo.RetryPeriod = &opts.RetryPeriod
	}
	mgr, err := ctrl.NewManager(restCfg, mo)
	if err != nil {
		return nil, nil, fmt.Errorf("create manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return nil, nil, err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return nil, nil, err
	}

	m := opts.Metrics
	if m == nil {
		m = metrics.NewControlPlane()
	}
	if err := m.Register(opts.Registry); err != nil {
		return nil, nil, fmt.Errorf("register metrics: %w", err)
	}
	m.SetDryRun(cp.DryRun)
	if err := metrics.RegisterBuildInfo(opts.Registry, metrics.ComponentControlPlane, opts.Version, opts.Revision); err != nil {
		return nil, nil, fmt.Errorf("register build_info: %w", err)
	}

	c := mgr.GetClient()
	if opts.WrapClient != nil {
		c = opts.WrapClient(c)
	}
	kc, err := kube.NewClientCluster(kube.ClientOptions{
		Client: c, APIReader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorder(EventReporter),
		DriverNamespace: cp.CSINodeDriverNamespace, DriverSelector: driverSel,
	})
	if err != nil {
		return nil, nil, err
	}
	exec, err := NewExecutor(ExecutorOptions{Cluster: kc, Client: c, Metrics: m, DryRun: cp.DryRun, Clock: opts.Clock})
	if err != nil {
		return nil, nil, err
	}
	eng, err := NewEngine(EngineOptions{
		Client: c, APIReader: mgr.GetAPIReader(), Cluster: kc, Executor: exec, Metrics: m, Clock: opts.Clock, Config: cp,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := eng.SetupWithManager(mgr, opts.SkipControllerNameValidation); err != nil {
		return nil, nil, fmt.Errorf("set up controllers: %w", err)
	}
	return mgr, eng, nil
}

// stripPVC keeps only what pod -> PV resolution needs.
func stripPVC(obj any) (any, error) {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return obj, nil
	}
	return &corev1.PersistentVolumeClaim{
		TypeMeta: pvc.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID, ResourceVersion: pvc.ResourceVersion,
			DeletionTimestamp: pvc.DeletionTimestamp,
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pvc.Spec.VolumeName},
	}, nil
}
