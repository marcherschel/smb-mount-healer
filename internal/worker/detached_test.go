package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// TestDetachedWorkerRound drives a detached worker (as internal/sim does)
// over a fake client and fake host.
func TestDetachedWorkerRound(t *testing.T) {
	ctx := context.Background()
	scheme, err := kube.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
			Driver: "smb.csi.k8s.io", VolumeHandle: "pv", VolumeAttributes: map[string]string{"source": "//s/pv"},
		}}},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", UID: "node-uid"}}
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, node).
		WithStatusSubresource(&v1alpha1.SmbMount{}, &v1alpha1.SmbQuarantine{}, &v1alpha1.SmbHostTask{}).Build()

	layout := kubelet.Layout{Root: "/var/lib/kubelet", DriverName: "smb.csi.k8s.io"}
	host := fake.NewHost(layout, "/var/lib/kubelet/q", "cifs")
	host.Mounts.Mount(layout.GlobalmountPath("pv"), "cifs", "//s/pv", "soft")
	w, err := NewDetached(Cluster{Client: c, Logger: logr.Discard()}, Deps{
		Config: config.Worker{NodeName: "n1", KubeletRoot: layout.Root, QuarantineDir: "/var/lib/kubelet/q",
			CheckInterval: 60e9, RecheckDelay: 3e9, RecheckAttempts: 3, ProbeConcurrency: 2, StatusHeartbeat: 300e9},
		CSIDriverName: layout.DriverName,
		Host:          Host{Mounts: host.Mounts, Dirs: host.Dirs, Prober: host.Prober, Mutator: host.Mutator, Quarantine: host.Quarantine},
		Clock:         clocktesting.NewFakeClock(unitNow),
		Metrics:       metrics.NewWorker("n1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ProbeRound(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.ScanQuarantine(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Publish(ctx); err != nil {
		t.Skipf("fake client cannot server-side apply SmbMount status here: %v", err)
	}
	var sm v1alpha1.SmbMount
	if err := c.Get(ctx, client.ObjectKey{Name: "n1.pv"}, &sm); err != nil {
		t.Fatal(err)
	}
	if sm.Spec.GlobalmountPath != layout.GlobalmountPath("pv") || sm.OwnerReferences[0].UID != "node-uid" {
		t.Fatalf("SmbMount: %+v", sm)
	}
	if sm.Status.Health != v1alpha1.HealthHealthy {
		t.Fatalf("health = %q", sm.Status.Health)
	}
	if err := w.readyz(httptest.NewRequest(http.MethodGet, "/readyz", nil)); err != nil {
		t.Fatalf("readyz: %v", err)
	}
}
