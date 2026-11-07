//go:build e2e

package framework

import (
	"strings"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// BusyboxImage is the fixtures' busybox (manifests/fixtures/workloads.yaml).
const BusyboxImage = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// BarePodLabel marks pods created by StartBarePod.
const BarePodLabel = "smbmh-e2e/bare-pod"

// StartBarePod creates an unowned pod (no controller) on the agent node that
// mounts the PVCs of pvs and sleeps, waits until it is Running and Ready, and
// registers its deletion as a cleanup. Uses:
//   - a "pinner" that keeps a volume in use while a fixture pod is recreated,
//     so kubelet never unstages it (the WRONG_FSTYPE injection);
//   - an unowned co-tenant the healer cannot scale (S15).
func (f *F) StartBarePod(name string, pvs ...string) *corev1.Pod {
	f.T.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: FixturesNamespace,
			Labels:    map[string]string{BarePodLabel: "true", NameLabel: name},
		},
		Spec: corev1.PodSpec{
			NodeSelector:                  map[string]string{FixtureNodeLabel: "true"},
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](1000)},
			Containers: []corev1.Container{{
				Name:    "sleep",
				Image:   BusyboxImage,
				Command: []string{"sleep", "86400"},
			}},
		},
	}
	for _, pv := range pvs {
		vn := strings.TrimPrefix(pv, "storagebox-e2e-")
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: vn, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pv + "-claim"},
		}})
		p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: vn, MountPath: "/" + vn})
	}
	_, err := f.Kube.CoreV1().Pods(FixturesNamespace).Create(f.Ctx, p, metav1.CreateOptions{})
	f.G.Expect(err).NotTo(gomega.HaveOccurred(), "create bare pod %s", name)
	f.T.Cleanup(func() { f.DeleteBarePod(name) })
	var out *corev1.Pod
	f.G.Eventually(func(g gomega.Gomega) {
		var err error
		out, err = f.Kube.CoreV1().Pods(FixturesNamespace).Get(f.Ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(podReady(out)).To(gomega.BeTrue(), "bare pod %s ready (phase %s)", name, out.Status.Phase)
	}).Should(gomega.Succeed())
	return out
}

// DeleteBarePod deletes a bare pod immediately (grace period 0) and waits
// until it is gone from the API (idempotent).
func (f *F) DeleteBarePod(name string) {
	f.T.Helper()
	err := f.Kube.CoreV1().Pods(FixturesNamespace).Delete(f.Ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	if !apierrors.IsNotFound(err) {
		f.G.Expect(err).NotTo(gomega.HaveOccurred())
	}
	f.G.Eventually(func() bool {
		_, err := f.Kube.CoreV1().Pods(FixturesNamespace).Get(f.Ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}).WithTimeout(time.Minute).Should(gomega.BeTrue(), "bare pod %s gone", name)
}
