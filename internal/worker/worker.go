package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// EventRecorderName is the reporting controller of the worker's Events.
const EventRecorderName = "smb-mount-healer-worker"

// Host bundles the host-layer implementations the worker runs on: hostfs/linux
// plus a probe.Runner in production, hostfs/fake in tests and internal/sim.
type Host struct {
	Mounts     hostfs.MountTable
	Dirs       hostfs.DirInspector
	Prober     hostfs.Prober
	Mutator    hostfs.Mutator
	Quarantine hostfs.QuarantineInspector
}

// Deps are everything New needs besides the manager.
type Deps struct {
	Config        config.Worker
	CSIDriverName string
	Host          Host
	// Clock drives every cadence, timestamp and timeout. Defaults to clock.RealClock{}.
	Clock clock.Clock
	// Metrics are the worker collectors (registered by the caller). Required.
	Metrics *metrics.Worker
	// Recorder emits events.k8s.io Events. Nil disables Events.
	Recorder events.EventRecorder
	// ManualLoops leaves the prober, publisher and quarantine-scanner loops
	// out of the manager: the caller (internal/sim) drives ProbeRound,
	// Publish and ScanQuarantine itself. The HostTask controller and the
	// health checks are registered either way, but HostTaskReconciler then
	// completes tasks without waiting for a published probe round.
	ManualLoops bool
}

// Worker is the --worker mode wired into one controller-runtime manager.
type Worker struct {
	node   string
	cfg    config.Worker
	layout kubelet.Layout
	host   Host
	clock  clock.Clock
	m      *metrics.Worker
	rec    events.EventRecorder
	log    logr.Logger

	c     client.Client // cached reads, writes
	api   client.Reader // live reads (Node, SmbHostTask)
	cache cache.Cache

	exec   *HostExecutor
	manual bool

	Prober    *ProberRunnable
	Publisher *SmbMountPublisher
	Tasks     *HostTaskReconciler
	Scanner   *QuarantineScanner

	ownerMu sync.Mutex
	owner   *metav1.OwnerReference
}

// ManagerOptions returns the worker's manager options: no leader election,
// and a cache scoped to this node (pods by spec.nodeName, the operator's
// CRs by the smbhealer.herschel.io/node label; PVs cluster-wide). Managed
// fields are stripped from cached objects.
func ManagerOptions(cfg config.Config, scheme *runtime.Scheme) (manager.Options, error) {
	node := cfg.Worker.NodeName
	if errs := validation.IsValidLabelValue(node); node == "" || len(errs) > 0 {
		return manager.Options{}, fmt.Errorf("node name %q cannot be used as a label value: %v", node, errs)
	}
	byNode := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{v1alpha1.LabelNode: node})}
	return manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: cfg.Common.MetricsBindAddress},
		HealthProbeBindAddress: cfg.Common.HealthProbeBindAddress,
		LeaderElection:         false,
		Cache: cache.Options{
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}:              {Field: fields.OneTermEqualSelector("spec.nodeName", node)},
				&v1alpha1.SmbMount{}:       byNode,
				&v1alpha1.SmbHostTask{}:    byNode,
				&v1alpha1.SmbQuarantine{}:  byNode,
				&corev1.PersistentVolume{}: {},
			},
		},
	}, nil
}

// New wires the worker into mgr: informers, the SmbHostTask controller,
// health checks and (unless ManualLoops) the ProberRunnable,
// SmbMountPublisher and QuarantineScanner runnables. mgr's cache must be
// scoped as in ManagerOptions.
func New(mgr manager.Manager, d Deps) (*Worker, error) {
	w, err := newWorker(Cluster{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Cache:     mgr.GetCache(),
		Logger:    mgr.GetLogger(),
	}, d)
	if err != nil {
		return nil, err
	}
	// Register every informer up front so the initial cache sync covers them.
	ctx := context.Background()
	for _, obj := range []client.Object{&corev1.Pod{}, &corev1.PersistentVolume{}, &v1alpha1.SmbMount{},
		&v1alpha1.SmbHostTask{}, &v1alpha1.SmbQuarantine{}} {
		if _, err := w.cache.GetInformer(ctx, obj); err != nil {
			return nil, fmt.Errorf("worker: informer for %T: %w", obj, err)
		}
	}
	if err := w.Tasks.setup(mgr); err != nil {
		return nil, err
	}
	if !d.ManualLoops {
		for _, r := range []manager.Runnable{w.Prober, w.Publisher, w.Scanner} {
			if err := mgr.Add(r); err != nil {
				return nil, err
			}
		}
	}
	if err := mgr.AddHealthzCheck("prober", w.healthz); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("worker", w.readyz); err != nil {
		return nil, err
	}
	return w, nil
}

// Cluster is the API access of a detached worker.
type Cluster struct {
	// Client reads Pods (of this node), PVs, and this node's SmbMounts,
	// SmbHostTasks and SmbQuarantines, and writes the worker's objects. In
	// production it reads from the node-scoped cache (ManagerOptions).
	Client client.Client
	// APIReader reads the Node and SmbHostTasks live. Defaults to Client.
	APIReader client.Reader
	// Cache provides informers for the loops; unused (may be nil) by a
	// detached worker.
	Cache  cache.Cache
	Logger logr.Logger
}

// NewDetached returns a worker without a manager (internal/sim): nothing
// runs on its own. The caller drives ProbeRound, Publish, ScanQuarantine and
// Tasks.Reconcile on its own schedule; ManualLoops is implied, so
// Tasks.Reconcile does not wait for a published probe round.
func NewDetached(cl Cluster, d Deps) (*Worker, error) {
	d.ManualLoops = true
	if cl.Client == nil {
		return nil, errors.New("worker: Cluster.Client is required")
	}
	if cl.APIReader == nil {
		cl.APIReader = cl.Client
	}
	return newWorker(cl, d)
}

func newWorker(cl Cluster, d Deps) (*Worker, error) {
	if d.Metrics == nil {
		return nil, errors.New("worker: Metrics is required")
	}
	h := d.Host
	if h.Mounts == nil || h.Dirs == nil || h.Prober == nil || h.Mutator == nil || h.Quarantine == nil {
		return nil, errors.New("worker: every Host implementation is required")
	}
	if d.Clock == nil {
		d.Clock = clock.RealClock{}
	}
	w := &Worker{
		node:   d.Config.NodeName,
		cfg:    d.Config,
		layout: kubelet.Layout{Root: d.Config.KubeletRoot, DriverName: d.CSIDriverName},
		host:   h,
		clock:  d.Clock,
		m:      d.Metrics,
		rec:    d.Recorder,
		log:    cl.Logger.WithName("worker"),
		c:      cl.Client,
		api:    cl.APIReader,
		cache:  cl.Cache,
		manual: d.ManualLoops,
	}
	w.Prober = newProber(w)
	w.Publisher = newPublisher(w)
	w.Scanner = newScanner(w)
	w.Tasks = &HostTaskReconciler{w: w}
	w.exec = &HostExecutor{
		KubeletRoot:   w.layout.Root,
		QuarantineDir: d.Config.QuarantineDir,
		Mounts:        h.Mounts,
		Dirs:          h.Dirs,
		Prober:        h.Prober,
		Mutator:       h.Mutator,
		Clock:         d.Clock,
		ProbeNow:      w.Prober.RequestProbe,
		OnQuarantined: func(string) { w.Scanner.Trigger() },
	}
	return w, nil
}

// Executor returns the host executor (shared with the HostTask controller).
func (w *Worker) Executor() *HostExecutor { return w.exec }

// ProbeRound runs one prober round (see ProberRunnable.Round) and hands the
// result to the publisher. For ManualLoops callers.
func (w *Worker) ProbeRound(ctx context.Context) error {
	_, err := w.Prober.Round(ctx)
	return err
}

// Publish runs one SmbMountPublisher sync. For ManualLoops callers.
func (w *Worker) Publish(ctx context.Context) error { return w.Publisher.Publish(ctx) }

// ScanQuarantine runs one QuarantineScanner scan. For ManualLoops callers.
func (w *Worker) ScanQuarantine(ctx context.Context) error { return w.Scanner.Scan(ctx) }

// logger returns the logger in ctx, else the worker's logger.
func (w *Worker) logger(ctx context.Context) logr.Logger {
	if l, err := logr.FromContext(ctx); err == nil {
		return l
	}
	return w.log
}

// nodeOwner returns the ownerReference to this worker's Node
// (blockOwnerDeletion=false), read once through the API reader.
func (w *Worker) nodeOwner(ctx context.Context) (metav1.OwnerReference, error) {
	w.ownerMu.Lock()
	defer w.ownerMu.Unlock()
	if w.owner != nil {
		return *w.owner, nil
	}
	var n corev1.Node
	if err := w.api.Get(ctx, client.ObjectKey{Name: w.node}, &n); err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("get node %s: %w", w.node, err)
	}
	w.owner = &metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Node",
		Name:               n.Name,
		UID:                n.UID,
		BlockOwnerDeletion: ptr.To(false),
	}
	return *w.owner, nil
}

// objectLabels are the labels of SmbMount and SmbQuarantine objects. The PV
// label is left out when the PV name is not a valid label value (> 63 chars).
func (w *Worker) objectLabels(pv string) map[string]string {
	l := map[string]string{v1alpha1.LabelNode: w.node}
	if pv != "" && len(validation.IsValidLabelValue(pv)) == 0 {
		l[v1alpha1.LabelPV] = pv
	}
	return l
}

// event records an Event if a recorder is configured. note is passed
// verbatim; it must never contain names from quarantined data.
func (w *Worker) event(regarding runtime.Object, eventType, reason, note string) {
	if w.rec == nil || regarding == nil {
		return
	}
	w.rec.Eventf(regarding, nil, eventType, reason, reason, "%s", note)
}

// healthz fails when the prober loop has not ticked for too long.
func (w *Worker) healthz(_ *http.Request) error {
	if w.manual {
		return nil
	}
	last := w.Prober.lastTick()
	if stale := w.clock.Since(last); stale > w.Prober.staleAfter() {
		return fmt.Errorf("prober loop has not ticked for %s", stale.Round(time.Second))
	}
	return nil
}

// readyz is ready once the caches are synced and the first probe round was
// published and the first quarantine scan finished.
func (w *Worker) readyz(req *http.Request) error {
	ctx, cancel := context.WithTimeout(req.Context(), time.Second)
	defer cancel()
	if w.cache != nil && !w.cache.WaitForCacheSync(ctx) {
		return errors.New("caches not synced")
	}
	if !w.Publisher.publishedOnce() {
		return errors.New("first probe round not published yet")
	}
	if !w.Scanner.scannedOnce() {
		return errors.New("first quarantine scan not finished yet")
	}
	return nil
}

// signal is a coalescing, non-blocking wake-up.
type signal chan struct{}

func newSignal() signal { return make(signal, 1) }

func (s signal) notify() {
	select {
	case s <- struct{}{}:
	default:
	}
}
