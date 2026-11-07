//go:build e2e

package framework

import (
	"fmt"
	"strconv"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// MountOptions are the PV/StorageClass mountOptions of manifests/fixtures
// (manifests/fixtures/storage.yaml).
var MountOptions = []string{
	"uid=1000", "gid=1000", "file_mode=0775", "dir_mode=0775", "vers=3.0", "cache=strict", "noserverino",
}

// Volume is one fixture PV (name == volumeHandle).
type Volume struct {
	PV     string // storagebox-e2e-<share>
	Server string // samba Deployment/Service name
	Share  string
	// MountPath inside the workload pod.
	MountPath string
}

// Workload is one fixture workload in smbmh-fixtures (manifests/fixtures/workloads.yaml).
type Workload struct {
	Kind     v1alpha1.WorkloadKind
	Name     string
	Replicas int32
	Volumes  []Volume
}

// PV returns the name of the fixture PV for share.
func PV(share string) string { return "storagebox-e2e-" + share }

func vol(server, share, mountPath string) Volume {
	return Volume{PV: PV(share), Server: server, Share: share, MountPath: mountPath}
}

// Workloads is the baseline: kind, replicas and volumes of every fixture
// workload. Keep in sync with manifests/fixtures/workloads.yaml.
var Workloads = []Workload{
	{Kind: v1alpha1.WorkloadKindDeployment, Name: "deploy-single", Replicas: 1, Volumes: []Volume{vol("samba", "config-a", "/data")}},
	{Kind: v1alpha1.WorkloadKindStatefulSet, Name: "sts-single", Replicas: 1, Volumes: []Volume{vol("samba", "config-b", "/data")}},
	{Kind: v1alpha1.WorkloadKindDeployment, Name: "two-vol", Replicas: 1, Volumes: []Volume{
		vol("samba", "config-c", "/config"), vol("samba", "dl", "/downloads"),
	}},
	{Kind: v1alpha1.WorkloadKindDeployment, Name: "cotenant-a", Replicas: 1, Volumes: []Volume{vol("samba", "library", "/data")}},
	{Kind: v1alpha1.WorkloadKindDeployment, Name: "cotenant-b", Replicas: 1, Volumes: []Volume{vol("samba", "library", "/data")}},
	{Kind: v1alpha1.WorkloadKindDeployment, Name: "landmine-app", Replicas: 0, Volumes: []Volume{vol("samba-landmine", "landmine", "/data")}},
	{Kind: v1alpha1.WorkloadKindDeployment, Name: "flux-fighter", Replicas: 1},
}

// StagedPVs are the PVs a healthy baseline has staged on the agent node
// (every PV of a workload with replicas > 0), sorted, deduplicated.
func StagedPVs() []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range Workloads {
		if w.Replicas == 0 {
			continue
		}
		for _, v := range w.Volumes {
			if !seen[v.PV] {
				seen[v.PV] = true
				out = append(out, v.PV)
			}
		}
	}
	return out
}

// Fixtures manipulates the fixture workloads and their volumes.
type Fixtures struct{ f *F }

// Layout is the kubelet path layout on the nodes.
var Layout = kubelet.Layout{Root: KubeletRoot, DriverName: CSIDriverName}

// Workload looks up a fixture workload by name (fails the test if unknown).
func (x *Fixtures) Workload(name string) Workload {
	x.f.T.Helper()
	for _, w := range Workloads {
		if w.Name == name {
			return w
		}
	}
	x.f.T.Fatalf("unknown fixture workload %q", name)
	return Workload{}
}

// Credentials returns the samba / node-stage credentials (Secret in kube-system).
func (x *Fixtures) Credentials() (user, password string) {
	x.f.T.Helper()
	s, err := x.f.Kube.CoreV1().Secrets(CSINamespace).Get(x.f.Ctx, "storagebox-e2e-credentials", metav1.GetOptions{})
	x.f.G.Expect(err).NotTo(gomega.HaveOccurred())
	return string(s.Data["username"]), string(s.Data["password"])
}

// GlobalmountPath is the staging path of pv on the node:
// /var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/<sha256(handle)>/globalmount.
func (x *Fixtures) GlobalmountPath(pv string) string { return Layout.GlobalmountPath(pv) }

// PodVolumePath is the publish path of pv for pod uid.
func (x *Fixtures) PodVolumePath(uid types.UID, pv string) string {
	return Layout.PodVolumeMountPath(uid, pv)
}

// DetachGlobalmount lazily unmounts pv's globalmount on the agent node, so the
// directory is left bare (incident 1/2 injection: kubelet's next publish
// bind-mounts the underlying local dir).
func (x *Fixtures) DetachGlobalmount(pv string) {
	x.f.T.Helper()
	x.f.Chaos.LazyUnmount(x.f.AgentNode, x.GlobalmountPath(pv))
}

// Pods returns the non-terminating pods of a fixture workload.
func (x *Fixtures) Pods(workload string) []corev1.Pod {
	x.f.T.Helper()
	ps, err := x.f.pods(FixturesNamespace, NameLabel+"="+workload, "")
	x.f.G.Expect(err).NotTo(gomega.HaveOccurred())
	return ps
}

// DeletePod deletes every current pod of workload (the controller recreates
// them) and returns the deleted pods' UIDs.
func (x *Fixtures) DeletePod(workload string) []types.UID {
	x.f.T.Helper()
	var uids []types.UID
	for _, p := range x.Pods(workload) {
		err := x.f.Kube.CoreV1().Pods(FixturesNamespace).Delete(x.f.Ctx, p.Name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			x.f.G.Expect(err).NotTo(gomega.HaveOccurred())
		}
		uids = append(uids, p.UID)
	}
	return uids
}

// Scale sets a fixture workload's replicas through the scale subresource.
func (x *Fixtures) Scale(workload string, replicas int32) {
	x.f.T.Helper()
	w := x.Workload(workload)
	if w.Kind == v1alpha1.WorkloadKindStatefulSet {
		scaleStatefulSet(x.f, FixturesNamespace, w.Name, replicas)
		return
	}
	scaleDeployment(x.f, FixturesNamespace, w.Name, replicas)
}

// Replicas returns spec.replicas and status.readyReplicas of a fixture workload.
func (x *Fixtures) Replicas(workload string) (spec, ready int32, err error) {
	w := x.Workload(workload)
	if w.Kind == v1alpha1.WorkloadKindStatefulSet {
		s, err := x.f.Kube.AppsV1().StatefulSets(FixturesNamespace).Get(x.f.Ctx, w.Name, metav1.GetOptions{})
		if err != nil {
			return 0, 0, err
		}
		return deref(s.Spec.Replicas), s.Status.ReadyReplicas, nil
	}
	d, err := x.f.Kube.AppsV1().Deployments(FixturesNamespace).Get(x.f.Ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, err
	}
	return deref(d.Spec.Replicas), d.Status.ReadyReplicas, nil
}

// EnableFighter makes flux-fighter keep target (e.g. "deployment/cotenant-a")
// at replicas, and registers DisableFighter as a cleanup.
func (x *Fixtures) EnableFighter(target string, replicas int32) {
	x.f.T.Helper()
	x.patchFighter(map[string]string{"enabled": "true", "target": target, "replicas": strconv.Itoa(int(replicas))})
	x.f.T.Cleanup(x.DisableFighter)
}

// DisableFighter turns flux-fighter off (idempotent).
func (x *Fixtures) DisableFighter() {
	x.f.T.Helper()
	x.patchFighter(map[string]string{"enabled": "false"})
}

// FighterEnabled reports the fighter's switch.
func (x *Fixtures) FighterEnabled() (bool, error) {
	cm, err := x.f.Kube.CoreV1().ConfigMaps(FixturesNamespace).Get(x.f.Ctx, "flux-fighter", metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return cm.Data["enabled"] == "true", nil
}

func (x *Fixtures) patchFighter(data map[string]string) {
	x.f.T.Helper()
	x.f.G.Eventually(func() error {
		cm, err := x.f.Kube.CoreV1().ConfigMaps(FixturesNamespace).Get(x.f.Ctx, "flux-fighter", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		for k, v := range data {
			cm.Data[k] = v
		}
		_, err = x.f.Kube.CoreV1().ConfigMaps(FixturesNamespace).Update(x.f.Ctx, cm, metav1.UpdateOptions{})
		return err
	}).Should(gomega.Succeed())
}

// Heartbeat reads hb-<pod> of every pod of workload from the samba side, per
// volume. Keys are "<share>/hb-<pod>". Missing files are an error.
func (x *Fixtures) Heartbeat(workload string) (map[string]string, error) {
	x.f.T.Helper()
	w := x.Workload(workload)
	out := map[string]string{}
	pods := x.Pods(workload)
	if len(pods) == 0 {
		return nil, fmt.Errorf("workload %s has no pods", workload)
	}
	for _, p := range pods {
		for _, v := range w.Volumes {
			srv := x.f.Samba
			if v.Server == x.f.Landmine.Name {
				srv = x.f.Landmine
			}
			file := "hb-" + p.Name
			c, err := srv.Cat(v.Share, file)
			if err != nil {
				return nil, err
			}
			out[v.Share+"/"+file] = c
		}
	}
	return out, nil
}

func deref(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

func scaleDeployment(f *F, ns, name string, replicas int32) {
	f.T.Helper()
	f.G.Eventually(func() error {
		s, err := f.Kube.AppsV1().Deployments(ns).GetScale(f.Ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		s.Spec.Replicas = replicas
		_, err = f.Kube.AppsV1().Deployments(ns).UpdateScale(f.Ctx, name, s, metav1.UpdateOptions{})
		return err
	}).Should(gomega.Succeed())
}

func scaleStatefulSet(f *F, ns, name string, replicas int32) {
	f.T.Helper()
	f.G.Eventually(func() error {
		s, err := f.Kube.AppsV1().StatefulSets(ns).GetScale(f.Ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		s.Spec.Replicas = replicas
		_, err = f.Kube.AppsV1().StatefulSets(ns).UpdateScale(f.Ctx, name, s, metav1.UpdateOptions{})
		return err
	}).Should(gomega.Succeed())
}
