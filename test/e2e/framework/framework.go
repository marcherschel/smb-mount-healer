//go:build e2e

package framework

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/marcherschel/smb-mount-healer/internal/kube"
)

// Namespaces and labels shared with the manifests under test/e2e/manifests
// and the Helm chart (production/smb-mount-healer, release smb-mount-healer).
const (
	HealerNamespace   = "smb-mount-healer-system"
	FixturesNamespace = "smbmh-fixtures"
	SambaNamespace    = "smbmh-samba"
	ChaosNamespace    = "smbmh-chaos"
	CSINamespace      = "kube-system"

	// FixtureNodeLabel marks the agent node (k3d.yaml) that runs every fixture.
	FixtureNodeLabel = "e2e.smbhealer.herschel.io/fixtures"

	// ComponentLabel selects the healer pods (chart component labels).
	ComponentLabel = "app.kubernetes.io/component"
	// NameLabel selects fixture, samba and chaos pods.
	NameLabel = "app.kubernetes.io/name"

	CSIDriverName  = "smb.csi.k8s.io"
	KubeletRoot    = "/var/lib/kubelet"
	CSINodeLabel   = "app=csi-smb-node"
	QuarantineRoot = KubeletRoot + "/smb-mount-healer-quarantine"
)

// Default timeouts. Remediations walk a dozen stages with the shortened
// values-e2e.yaml waits, so they take minutes, not seconds.
const (
	DefaultPoll             = 2 * time.Second
	DefaultTimeout          = 2 * time.Minute
	RemediationTimeout      = 12 * time.Minute
	DefaultBaselineTimeout  = 10 * time.Minute
	DefaultConsistentWindow = 30 * time.Second
)

// F is the per-test handle: clients, Gomega bound to t, and the helpers.
// Create one per test with New; it registers WaitBaseline as a t.Cleanup.
type F struct {
	T      *testing.T
	Ctx    context.Context
	Config *rest.Config
	Kube   kubernetes.Interface
	Client client.Client
	G      *gomega.WithT

	// AgentNode is the node carrying FixtureNodeLabel (k3d-<cluster>-agent-0).
	AgentNode string

	Chaos    *Chaos
	Samba    *Samba
	Landmine *Samba
	Fixtures *Fixtures

	allowQuarantines bool
	clearQuarantines bool
	// remediationsAtStart are the SmbMountRemediation names that existed
	// when the test started; WaitRemediation only looks at newer ones.
	remediationsAtStart map[string]bool
}

// The suite only uses controller-runtime's client; silence its logger.
func init() { ctrllog.SetLogger(logr.Discard()) }

// Option tunes New.
type Option func(*F)

// AllowQuarantines lets SmbQuarantine objects exist at baseline (tests that
// create quarantines on purpose, S6/S7/S17). Such tests clean up the
// quarantine dir themselves before the baseline wait, or accept leftovers.
func AllowQuarantines() Option { return func(f *F) { f.allowQuarantines = true } }

// New connects to the e2e cluster (KUBECONFIG, context E2E_KUBE_CONTEXT if
// set, else the current context), resolves the agent node, deletes the
// terminal remediations of earlier tests (the backoff history, see
// forgetHistory), and registers WaitBaseline as a cleanup so every test
// leaves the cluster healthy.
func New(t *testing.T, opts ...Option) *F {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg, err := restConfig()
	if err != nil {
		t.Fatalf("e2e: kube config: %v", err)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("e2e: clientset: %v", err)
	}
	scheme, err := kube.NewScheme()
	if err != nil {
		t.Fatalf("e2e: scheme: %v", err)
	}
	cc, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("e2e: controller-runtime client: %v", err)
	}

	f := &F{
		T:      t,
		Ctx:    ctx,
		Config: cfg,
		Kube:   kc,
		Client: cc,
		G:      gomega.NewWithT(t),
	}
	for _, o := range opts {
		o(f)
	}
	f.G.SetDefaultEventuallyTimeout(DefaultTimeout)
	f.G.SetDefaultEventuallyPollingInterval(DefaultPoll)
	f.G.SetDefaultConsistentlyDuration(DefaultConsistentWindow)
	f.G.SetDefaultConsistentlyPollingInterval(DefaultPoll)

	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: FixtureNodeLabel + "=true"})
	if err != nil {
		t.Fatalf("e2e: list nodes: %v", err)
	}
	if len(nodes.Items) != 1 {
		t.Fatalf("e2e: want exactly one node labelled %s=true, got %d", FixtureNodeLabel, len(nodes.Items))
	}
	f.AgentNode = nodes.Items[0].Name

	f.Chaos = &Chaos{f: f}
	f.Samba = &Samba{f: f, Name: "samba"}
	f.Landmine = &Samba{f: f, Name: "samba-landmine"}
	f.Fixtures = &Fixtures{f: f}

	f.forgetHistory()
	f.remediationsAtStart = map[string]bool{}
	for _, r := range f.Remediations() {
		f.remediationsAtStart[r.Name] = true
	}

	t.Cleanup(f.WaitBaseline)
	if f.clearQuarantines {
		// Runs before WaitBaseline (cleanups are LIFO).
		t.Cleanup(f.RemoveQuarantineFolders)
	}
	return f
}

func restConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if c := os.Getenv("E2E_KUBE_CONTEXT"); c != "" {
		overrides.CurrentContext = c
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, err
	}
	// The suite polls a lot; keep client-side throttling out of the way.
	cfg.QPS = 50
	cfg.Burst = 100
	return cfg, nil
}

// Logf logs through the test.
func (f *F) Logf(format string, args ...any) {
	f.T.Helper()
	f.T.Logf(format, args...)
}

// pods lists pods by label selector (and node, if set), skipping terminating ones.
func (f *F) pods(ns, selector, node string) ([]corev1.Pod, error) {
	opts := metav1.ListOptions{LabelSelector: selector}
	if node != "" {
		opts.FieldSelector = "spec.nodeName=" + node
	}
	l, err := f.Kube.CoreV1().Pods(ns).List(f.Ctx, opts)
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Pod, 0, len(l.Items))
	for _, p := range l.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// readyPod returns the single ready, non-terminating pod matching selector
// (on node, if set), or an error describing what was found instead.
func (f *F) readyPod(ns, selector, node string) (*corev1.Pod, error) {
	ps, err := f.pods(ns, selector, node)
	if err != nil {
		return nil, err
	}
	var ready []corev1.Pod
	var seen []string
	for _, p := range ps {
		seen = append(seen, fmt.Sprintf("%s(%s)", p.Name, p.Status.Phase))
		if podReady(&p) {
			ready = append(ready, p)
		}
	}
	if len(ready) != 1 {
		return nil, fmt.Errorf("want 1 ready pod %s/%s on node %q, got %d (pods: %s)",
			ns, selector, node, len(ready), strings.Join(seen, ", "))
	}
	return &ready[0], nil
}

// ReadyPod is readyPod wrapped in Eventually, for callers that need the pod
// to settle (e.g. right after a restart).
func (f *F) ReadyPod(ns, selector, node string) *corev1.Pod {
	f.T.Helper()
	var pod *corev1.Pod
	f.G.Eventually(func() error {
		var err error
		pod, err = f.readyPod(ns, selector, node)
		return err
	}).Should(gomega.Succeed())
	return pod
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
