//go:build envtest

package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

const (
	testDriver     = "smb.csi.k8s.io"
	testRoot       = "/var/lib/kubelet"
	testQuarantine = "/var/lib/kubelet/smb-mount-healer-quarantine"
	waitTimeout    = 30 * time.Second
)

var (
	restCfg    *rest.Config
	testScheme *runtime.Scheme
	k8s        client.Client
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	if restCfg, err = env.Start(); err != nil {
		panic("starting envtest (run via `make test-envtest`): " + err.Error())
	}
	if testScheme, err = kube.NewScheme(); err != nil {
		panic(err)
	}
	if k8s, err = client.New(restCfg, client.Options{Scheme: testScheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// logSink captures every log line of a harness (privacy assertions).
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) add(prefix, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, prefix+" "+args)
}

func (s *logSink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

// harness is one test node: a Node object, a fake host and a fake clock
// that survive worker restarts, plus the currently running worker.
type harness struct {
	t      *testing.T
	g      *WithT
	ctx    context.Context
	node   string
	ns     string
	layout kubelet.Layout
	host   *fake.Host
	clock  *clocktesting.FakeClock
	cfg    config.Config
	logs   *logSink
	log    logr.Logger

	w      *Worker
	m      *metrics.Worker
	reg    *prometheus.Registry
	cancel context.CancelFunc
	done   chan error
}

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	h := &harness{
		t:      t,
		g:      NewWithT(t),
		ctx:    ctx,
		node:   "node-" + randSuffix(),
		layout: kubelet.Layout{Root: testRoot, DriverName: testDriver},
		// An hour ahead of the API server, so every pod is past POD_STARTUP_GRACE.
		clock: clocktesting.NewFakeClock(time.Now().Add(time.Hour).Truncate(time.Second)),
		logs:  &logSink{},
	}
	h.g.SetDefaultEventuallyTimeout(waitTimeout)
	h.g.SetDefaultEventuallyPollingInterval(50 * time.Millisecond)
	h.log = funcr.New(h.logs.add, funcr.Options{Verbosity: 2})
	h.host = fake.NewHost(h.layout, testQuarantine, "cifs", "smb3")
	h.cfg = config.Config{
		Mode: config.ModeWorker,
		Common: config.Common{
			MetricsBindAddress:     "0",
			HealthProbeBindAddress: "0",
			CSIDriverName:          testDriver,
		},
		Worker: config.Worker{
			NodeName:                 h.node,
			KubeletRoot:              testRoot,
			ExpectedFSTypes:          []string{"cifs", "smb3"},
			CheckInterval:            60 * time.Second,
			RecheckAttempts:          3,
			RecheckDelay:             3 * time.Second,
			StatTimeout:              5 * time.Second,
			StartupGrace:             0,
			PodStartupGrace:          0,
			SlowConfirmWindow:        3 * time.Minute,
			ProbeConcurrency:         4,
			StatusHeartbeat:          5 * time.Minute,
			QuarantineDir:            testQuarantine,
			QuarantineScanInterval:   5 * time.Minute,
			QuarantineScanMaxEntries: 1000,
			QuarantineScanTimeBudget: 10 * time.Second,
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: h.node}}
	h.g.Expect(k8s.Create(ctx, node)).To(Succeed())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "worker-test-"}}
	h.g.Expect(k8s.Create(ctx, ns)).To(Succeed())
	h.ns = ns.Name
	t.Cleanup(h.stop)
	return h
}

// fakeHost returns the worker Host over h.host.
func (h *harness) fakeHost() Host {
	return Host{Mounts: h.host.Mounts, Dirs: h.host.Dirs, Prober: h.host.Prober, Mutator: h.host.Mutator, Quarantine: h.host.Quarantine}
}

// start runs a new worker incarnation (new manager, cache, metrics) on host
// and waits until it is ready.
func (h *harness) start(host Host) {
	h.t.Helper()
	mopts, err := ManagerOptions(h.cfg, testScheme)
	h.g.Expect(err).NotTo(HaveOccurred())
	mopts.Logger = h.log
	mgr, err := ctrl.NewManager(restCfg, mopts)
	h.g.Expect(err).NotTo(HaveOccurred())
	h.reg = prometheus.NewRegistry()
	h.m = metrics.NewWorker(h.node)
	h.g.Expect(h.m.Register(h.reg)).To(Succeed())
	h.w, err = New(mgr, Deps{
		Config:        h.cfg.Worker,
		CSIDriverName: testDriver,
		Host:          host,
		Clock:         h.clock,
		Metrics:       h.m,
		Recorder:      mgr.GetEventRecorder(EventRecorderName),
	})
	h.g.Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithCancel(h.ctx)
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- mgr.Start(ctx) }()
	h.g.Eventually(func() error {
		return h.w.readyz(httptest.NewRequest(http.MethodGet, "/readyz", nil))
	}).Should(Succeed())
	h.g.Expect(h.w.healthz(nil)).To(Succeed())
}

// stop stops the running worker incarnation, if any.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		h.g.Expect(err).NotTo(HaveOccurred())
	case <-time.After(waitTimeout):
		h.t.Error("manager did not stop")
	}
	h.cancel = nil
}

// restart stops the worker and starts a new incarnation on the fake host.
func (h *harness) restart() {
	h.stop()
	h.start(h.fakeHost())
}

// probeAll makes every mount due, and waits until that round was published.
func (h *harness) probeAll() {
	h.t.Helper()
	seq := h.w.Prober.nextRoundSeq()
	h.w.Prober.RequestProbe("")
	ctx, cancel := context.WithTimeout(h.ctx, waitTimeout)
	defer cancel()
	h.g.Expect(h.w.Publisher.waitPublished(ctx, seq, time.Hour)).To(BeTrue(), "round %d not published", seq)
}

// pv creates an smb PV bound to claim in h.ns.
func (h *harness) pv(name, claim string) *corev1.PersistentVolume {
	h.t.Helper()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver:           testDriver,
				VolumeHandle:     name,
				VolumeAttributes: map[string]string{"source": "//samba.example/" + name},
			}},
		},
	}
	if claim != "" {
		pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: h.ns, Name: claim}
	}
	h.g.Expect(k8s.Create(h.ctx, pv)).To(Succeed())
	h.t.Cleanup(func() { _ = k8s.Delete(context.Background(), pv) })
	return pv
}

// pod creates a pod on h.node that mounts claim.
func (h *harness) pod(name, claim string) *corev1.Pod {
	h.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.ns},
		Spec: corev1.PodSpec{
			NodeName:   h.node,
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			}}},
		},
	}
	h.g.Expect(k8s.Create(h.ctx, pod)).To(Succeed())
	return pod
}

func (h *harness) smbMount(pv string) *v1alpha1.SmbMount {
	h.t.Helper()
	var sm v1alpha1.SmbMount
	h.g.Expect(k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, pv)}, &sm)).To(Succeed())
	return &sm
}

// eventuallySmbMount waits until the SmbMount of pv satisfies check.
func (h *harness) eventuallySmbMount(pv string, check func(g Gomega, sm *v1alpha1.SmbMount)) *v1alpha1.SmbMount {
	h.t.Helper()
	var sm v1alpha1.SmbMount
	h.g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, pv)}, &sm)).To(Succeed())
		check(g, &sm)
	}).Should(Succeed())
	return &sm
}

func (h *harness) podPath(uid types.UID, pv string) string {
	return h.layout.PodVolumeMountPath(uid, pv)
}
