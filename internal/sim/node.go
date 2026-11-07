package sim

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/internal/worker"
)

// CSI driver DaemonSet timings.
const (
	driverRecreateDelay = 2 * time.Second
	driverReadyDelay    = 5 * time.Second
)

// Node is one simulated node: fake hostfs, fake kubelet, CSI driver pod and
// a detached worker.
type Node struct {
	Name    string
	w       *World
	Host    *fake.Host
	kubelet *Kubelet
	Worker  *worker.Worker
	wm      *metrics.Worker
	wc      client.WithWatch

	workerDown    bool
	workerUpAt    time.Time
	roundsFrom    time.Time // STARTUP_GRACE after a (re)start
	nextScan      time.Time
	scanRequested bool

	// CSI driver DaemonSet emulation.
	driverSeq     int
	driverMissing time.Time
	// driverGens is the samba address generation the running driver
	// resolved when it became ready (stale sessions until restarted).
	driverGens map[string]int
}

func newNode(w *World, name string) (*Node, error) {
	n := &Node{Name: name, w: w, Host: fake.NewHost(w.layout, QuarantineDir, "cifs", "smb3")}
	n.kubelet = newKubelet(n)
	n.wm = metrics.NewWorker(name)
	if err := n.wm.Register(w.reg); err != nil {
		return nil, err
	}
	n.wc = interceptor.NewClient(w.store, interceptor.Funcs{SubResourceUpdate: n.icWorkerStatusUpdate})
	return n, nil
}

// startWorker (re)creates the detached worker; restart applies STARTUP_GRACE
// before the first probe round, as the real ProberRunnable does.
func (n *Node) startWorker(restart bool) error {
	cfg := n.w.wkCfg
	cfg.NodeName = n.Name
	h := n.Host
	wk, err := worker.NewDetached(worker.Cluster{Client: n.wc, Logger: n.w.log.WithValues("node", n.Name)}, worker.Deps{
		Config:        cfg,
		CSIDriverName: DriverName,
		Host:          worker.Host{Mounts: h.Mounts, Dirs: h.Dirs, Prober: h.Prober, Mutator: h.Mutator, Quarantine: h.Quarantine},
		Clock:         n.w.Clock,
		Metrics:       n.wm,
		Recorder:      &recorder{w: n.w, source: "worker/" + n.Name},
	})
	if err != nil {
		return err
	}
	n.Worker = wk
	n.workerDown = false
	n.roundsFrom = n.w.Now()
	if restart {
		n.roundsFrom = n.w.Now().Add(cfg.StartupGrace)
		n.w.trace.add(Event{Kind: "worker", Obj: n.Name, Msg: "started (fresh process)"})
	}
	n.scanRequested = true
	return nil
}

// CrashWorker kills the worker process; it restarts after downtime with a
// fresh, stateless worker (the node's host state survives).
func (n *Node) CrashWorker(downtime time.Duration) {
	n.Worker = nil
	n.workerDown = true
	n.workerUpAt = n.w.Now().Add(downtime)
	n.w.trace.add(Event{Kind: "worker", Obj: n.Name, Msg: fmt.Sprintf("crashed (down %s)", downtime)})
}

// stepWorker runs the detached worker: pending host tasks first (each
// followed by a probe round, publish and quarantine scan, which the real
// HostTaskReconciler waits for), then the regular round, publish and scan.
func (n *Node) stepWorker() {
	w := n.w
	if n.workerDown {
		if w.Now().Before(n.workerUpAt) {
			return
		}
		if err := n.startWorker(true); err != nil {
			w.violation("restart worker %s: %v", n.Name, err)
			return
		}
	}
	inGrace := w.Now().Before(n.roundsFrom)
	if !inGrace {
		ran := n.reconcileTasks()
		if n.Worker == nil {
			return // crashed mid-task
		}
		if ran {
			n.round()
			n.scanRequested = true
		}
		n.round()
	}
	if n.scanRequested || !w.Now().Before(n.nextScan) {
		n.scanRequested = false
		n.nextScan = w.Now().Add(w.wkCfg.QuarantineScanInterval)
		if err := n.Worker.ScanQuarantine(w.ctx); err != nil {
			w.trace.errorf("worker/"+n.Name, "quarantine scan: %v", err)
		}
	}
}

func (n *Node) round() {
	w := n.w
	if _, err := n.Worker.Prober.Round(w.ctx); err != nil {
		w.trace.errorf("worker/"+n.Name, "probe round: %v", err)
	}
	if err := n.Worker.Publish(w.ctx); err != nil {
		w.trace.errorf("worker/"+n.Name, "publish: %v", err)
	}
}

// reconcileTasks runs Tasks.Reconcile for every unfinished task of this
// node, in name order. It reports whether any task made progress.
func (n *Node) reconcileTasks() (ran bool) {
	w := n.w
	var list v1alpha1.SmbHostTaskList
	if err := w.store.List(w.ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: n.Name}); err != nil {
		w.violation("list tasks: %v", err)
		return false
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		t := &list.Items[i]
		if t.Spec.NodeName != n.Name || !t.DeletionTimestamp.IsZero() {
			continue
		}
		if (t.Status.Phase == v1alpha1.HostTaskSucceeded || t.Status.Phase == v1alpha1.HostTaskFailed) &&
			t.Status.ObservedGeneration == t.Generation {
			continue
		}
		ran = true
		crashed := protect(func() {
			if _, err := n.Worker.Tasks.Reconcile(w.ctx, reconcileRequest(t.Name)); err != nil {
				w.trace.errorf("worker/"+n.Name, "host task %s: %v", t.Name, err)
			}
		})
		if crashed {
			n.CrashWorker(0)
			return true
		}
	}
	return ran
}

// --- CSI driver DaemonSet -------------------------------------------------

func (n *Node) driverPods() []corev1.Pod {
	w := n.w
	var list corev1.PodList
	if err := w.store.List(w.ctx, &list, client.InNamespace(DriverNS), client.MatchingLabelsSelector{Selector: driverSelector()}); err != nil {
		w.violation("list driver pods: %v", err)
		return nil
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.Spec.NodeName == n.Name {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// driverReady reports whether a Running+Ready driver pod serves the node.
func (n *Node) driverReady() bool {
	for _, p := range n.driverPods() {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && podReady(&p) {
			return true
		}
	}
	return false
}

// DriverGeneration is the number of driver pods created on the node.
func (n *Node) DriverGeneration() int { return n.driverSeq }

func (n *Node) stepDriver() {
	w := n.w
	now := w.Now()
	var live *corev1.Pod
	pods := n.driverPods()
	for i := range pods {
		if pods[i].DeletionTimestamp == nil {
			live = &pods[i]
		}
	}
	if live == nil {
		if n.driverMissing.IsZero() {
			n.driverMissing = now
		}
		if n.driverSeq > 0 && now.Sub(n.driverMissing) < driverRecreateDelay {
			return
		}
		n.driverSeq++
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: DriverNS, Name: fmt.Sprintf("csi-smb-node-%s-%d", n.Name, n.driverSeq),
				Labels: map[string]string{"app": driverLabel},
			},
			Spec:   corev1.PodSpec{NodeName: n.Name, Containers: []corev1.Container{{Name: "smb", Image: "csi-smb"}}},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		}
		if err := w.store.Create(w.ctx, p); err != nil {
			w.violation("create driver pod: %v", err)
			return
		}
		n.driverMissing = time.Time{}
		w.trace.add(Event{Kind: "driver", Obj: p.Name, Msg: "created"})
		if !w.started {
			// Initial DaemonSet pod: ready right away.
			n.makeDriverReady(p)
		}
		return
	}
	if live.Status.Phase != corev1.PodRunning && now.Sub(live.CreationTimestamp.Time) >= driverReadyDelay {
		n.makeDriverReady(live)
	}
}

func (n *Node) makeDriverReady(p *corev1.Pod) {
	w := n.w
	setPodRunning(p, w.Now())
	if err := w.store.Status().Update(w.ctx, p); err != nil {
		w.violation("driver pod status: %v", err)
		return
	}
	n.driverGens = w.Samba.gens()
	w.trace.add(Event{Kind: "driver", Obj: p.Name, Msg: "ready"})
}

// driverGen is the samba generation the running driver resolves for server.
func (n *Node) driverGen(server string) int {
	if g, ok := n.driverGens[server]; ok {
		return g
	}
	return 1
}
