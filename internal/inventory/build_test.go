package inventory

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

const (
	node   = "worker-1"
	driver = "smb.csi.k8s.io"
)

var (
	layout = kubelet.Layout{Root: "/var/lib/kubelet", DriverName: driver}
	now    = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	grace  = 2 * time.Minute
)

// smbPV builds a static smb PV shaped like the tofu ones (name == handle by default).
func smbPV(name, handle, share, claimNS, claim string) corev1.PersistentVolume {
	pv := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver:           driver,
				VolumeHandle:     handle,
				VolumeAttributes: map[string]string{"source": share},
			}},
		},
	}
	if claim != "" {
		pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: claimNS, Name: claim}
	}
	return pv
}

type podOpt func(*corev1.Pod)

func pod(ns, name, uid string, claims []string, opts ...podOpt) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, UID: types.UID(uid),
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	st := metav1.NewTime(now.Add(-time.Hour))
	p.Status.StartTime = &st
	for i, c := range claims {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name:         "v" + string(rune('a'+i)),
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c}},
		})
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func ownedBy(kind, name string) podOpt {
	return func(p *corev1.Pod) {
		yes := true
		p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
	}
}

func cifs(path string) hostfs.MountEntry {
	return hostfs.MountEntry{MountPoint: path, FSType: "cifs", SuperOptions: "rw,soft,vers=3.1.1"}
}

func gmDir(handle string) hostfs.GlobalmountDir {
	return hostfs.GlobalmountDir{Hash: kubelet.GlobalmountHash(handle), Path: layout.GlobalmountPath(handle)}
}

func podDir(uid, pv string) hostfs.PodVolumeDir {
	return hostfs.PodVolumeDir{PodUID: types.UID(uid), PVName: pv, Path: layout.PodVolumeMountPath(types.UID(uid), pv)}
}

func baseInput() Input {
	return Input{NodeName: node, Layout: layout, Now: now, PodStartupGrace: grace, DirFacts: map[string]hostfs.DirFacts{}}
}

func TestBuildHealthyPodAndGlobalmount(t *testing.T) {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{smbPV("sb-config", "sb-config", "//u1.example/config", "media", "config")}
	in.Pods = []corev1.Pod{pod("media", "sonarr-7d9f-abcde", "uid-1", []string{"config"},
		ownedBy("ReplicaSet", "sonarr-7d9f"), func(p *corev1.Pod) { p.Labels = map[string]string{"pod-template-hash": "7d9f"} })}
	gm := layout.GlobalmountPath("sb-config")
	pp := layout.PodVolumeMountPath("uid-1", "sb-config")
	in.Snapshot = hostfs.MountSnapshot{Entries: []hostfs.MountEntry{cifs(gm), cifs(pp)}}
	in.GlobalmountDirs = []hostfs.GlobalmountDir{gmDir("sb-config")}
	in.PodVolumeDirs = []hostfs.PodVolumeDir{podDir("uid-1", "sb-config")}

	want := NodeView{
		NodeName: node,
		Volumes: []VolumeView{{
			PVName: "sb-config", Share: "//u1.example/config", VolumeHandle: "sb-config",
			SmbMountName: v1alpha1.SmbMountName(node, "sb-config"),
			Globalmount:  GlobalmountView{Path: gm, Mounted: true, FSType: "cifs", SuperOptions: "rw,soft,vers=3.1.1"},
			PodMounts: []PodMountView{{
				Namespace: "media", Pod: "sonarr-7d9f-abcde", UID: "uid-1", Workload: "Deployment/sonarr",
				Path: pp, Mounted: true, FSType: "cifs",
			}},
		}},
	}
	got := Build(in)
	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("(-want +got):\n%s", d)
	}
	wantTargets := []ProbeTarget{
		{SmbMountName: "worker-1.sb-config", PVName: "sb-config", Kind: v1alpha1.MountKindGlobalmount, Path: gm},
		{SmbMountName: "worker-1.sb-config", PVName: "sb-config", Kind: v1alpha1.MountKindPod, PodUID: "uid-1", Path: pp},
	}
	if d := cmp.Diff(wantTargets, got.ProbeTargets()); d != "" {
		t.Fatalf("targets (-want +got):\n%s", d)
	}
	if d := DirsToInspect(in.Snapshot, in.PodVolumeDirs, in.GlobalmountDirs); len(d) != 0 {
		t.Fatalf("DirsToInspect = %v, want none (all mounted)", d)
	}
}

func TestBuildIgnoresNonSmbPVs(t *testing.T) {
	in := baseInput()
	nfs := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "nfs"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "x", Path: "/"}},
			ClaimRef:               &corev1.ObjectReference{Namespace: "a", Name: "nfs"},
		},
	}
	other := smbPV("longhorn", "lh-1", "", "a", "lh")
	other.Spec.CSI.Driver = "driver.longhorn.io"
	noHandle := smbPV("nohandle", "", "//s/x", "a", "nh")
	in.PVs = []corev1.PersistentVolume{nfs, other, noHandle}
	in.Pods = []corev1.Pod{pod("a", "p", "uid-1", []string{"nfs", "lh", "nh"})}
	in.PodVolumeDirs = []hostfs.PodVolumeDir{podDir("uid-1", "longhorn"), podDir("uid-9", "longhorn"), podDir("uid-9", "deleted-pv")}
	got := Build(in)
	if len(got.Volumes) != 0 || len(got.UnknownGlobalmounts) != 0 || len(got.ProbeTargets()) != 0 {
		t.Fatalf("expected empty view, got %+v", got)
	}
}

func TestBuildPVWithoutClaimRefGlobalmountOnly(t *testing.T) {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{
		smbPV("unbound", "unbound", "//s/unbound", "", ""),
		smbPV("absent", "absent", "//s/absent", "", ""), // nothing on this node
	}
	gm := layout.GlobalmountPath("unbound")
	in.Snapshot = hostfs.MountSnapshot{Entries: []hostfs.MountEntry{cifs(gm)}}
	got := Build(in)
	if len(got.Volumes) != 1 || got.Volumes[0].PVName != "unbound" || !got.Volumes[0].Globalmount.Mounted ||
		len(got.Volumes[0].PodMounts) != 0 {
		t.Fatalf("got %+v", got)
	}
	tg := got.ProbeTargets()
	if len(tg) != 1 || tg[0].Kind != v1alpha1.MountKindGlobalmount || tg[0].Path != gm {
		t.Fatalf("targets %+v", tg)
	}
}

func TestBuildMirrorPVsSameShareDifferentHandles(t *testing.T) {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{
		smbPV("dl-sonarr", "dl-sonarr", "//s/dl", "media", "dl"),
		smbPV("dl-radarr", "dl-radarr", "//s/dl", "movies", "dl"),
	}
	in.Pods = []corev1.Pod{
		pod("media", "sonarr", "uid-s", []string{"dl"}, ownedBy("StatefulSet", "sonarr")),
		pod("movies", "radarr", "uid-r", []string{"dl"}, ownedBy("ReplicaSet", "radarr-abc")),
	}
	gmS, gmR := layout.GlobalmountPath("dl-sonarr"), layout.GlobalmountPath("dl-radarr")
	in.Snapshot = hostfs.MountSnapshot{Entries: []hostfs.MountEntry{cifs(gmS), cifs(gmR)}}
	got := Build(in)
	if len(got.Volumes) != 2 {
		t.Fatalf("volumes %+v", got.Volumes)
	}
	r, s := got.Volumes[0], got.Volumes[1]
	if r.PVName != "dl-radarr" || s.PVName != "dl-sonarr" || r.Share != s.Share {
		t.Fatalf("order/share: %+v %+v", r, s)
	}
	if r.Globalmount.Path == s.Globalmount.Path {
		t.Fatal("mirror PVs must have distinct globalmounts")
	}
	if len(r.PodMounts) != 1 || r.PodMounts[0].UID != "uid-r" || r.PodMounts[0].Workload != "ReplicaSet/radarr-abc" {
		t.Fatalf("radarr pod mounts %+v", r.PodMounts)
	}
	if len(s.PodMounts) != 1 || s.PodMounts[0].UID != "uid-s" || s.PodMounts[0].Workload != "StatefulSet/sonarr" {
		t.Fatalf("sonarr pod mounts %+v", s.PodMounts)
	}
	// Pod mounts are not mounted here: still probe targets (not_mountpoint detection).
	if n := len(got.ProbeTargets()); n != 4 {
		t.Fatalf("targets = %d, want 4", n)
	}
}

func TestBuildOrphanPodDirs(t *testing.T) {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{
		smbPV("cfg", "cfg", "//s/cfg", "app", "cfg"),
		smbPV("dl", "dl", "//s/dl", "app", "dl"),
	}
	// live pod references cfg only; its dir for dl (claim changed) is an orphan.
	in.Pods = []corev1.Pod{pod("app", "live", "uid-live", []string{"cfg"})}
	livePath := layout.PodVolumeMountPath("uid-live", "cfg")
	deadMounted := podDir("uid-dead", "cfg")
	deadEmpty := podDir("uid-dead2", "cfg")
	deadShadow := podDir("uid-dead3", "dl")
	liveOtherPV := podDir("uid-live", "dl")
	in.PodVolumeDirs = []hostfs.PodVolumeDir{podDir("uid-live", "cfg"), deadMounted, deadEmpty, deadShadow, liveOtherPV, deadEmpty}
	in.Snapshot = hostfs.MountSnapshot{Entries: []hostfs.MountEntry{cifs(livePath), cifs(deadMounted.Path)}}
	in.DirFacts = map[string]hostfs.DirFacts{
		deadEmpty.Path:   {Exists: true, IsDir: true, Empty: true},
		deadShadow.Path:  {Exists: true, IsDir: true, Empty: false},
		liveOtherPV.Path: {Exists: true, IsDir: true, Empty: true},
	}
	got := Build(in)
	want := map[string][]OrphanDirView{
		"cfg": sortedOrphans(
			OrphanDirView{UID: "uid-dead", Path: deadMounted.Path, Mounted: true, FSType: "cifs"},
			OrphanDirView{UID: "uid-dead2", Path: deadEmpty.Path, Dir: hostfs.DirFacts{Exists: true, IsDir: true, Empty: true}},
		),
		"dl": sortedOrphans(
			OrphanDirView{UID: "uid-dead3", Path: deadShadow.Path, Dir: hostfs.DirFacts{Exists: true, IsDir: true}},
			OrphanDirView{UID: "uid-live", Path: liveOtherPV.Path, Dir: hostfs.DirFacts{Exists: true, IsDir: true, Empty: true}},
		),
	}
	if len(got.Volumes) != 2 {
		t.Fatalf("volumes %+v", got.Volumes)
	}
	for _, v := range got.Volumes {
		if d := cmp.Diff(want[v.PVName], v.OrphanDirs); d != "" {
			t.Errorf("%s orphans (-want +got):\n%s", v.PVName, d)
		}
	}
	if len(got.Volumes[1].PodMounts) != 0 {
		t.Errorf("dl must have no pod mounts: %+v", got.Volumes[1].PodMounts)
	}
	// Probe targets: cfg live pod mount + mounted orphan only.
	tg := got.ProbeTargets()
	paths := []string{}
	for _, x := range tg {
		paths = append(paths, x.Path)
	}
	wantPaths := sortedStrings([]string{livePath, deadMounted.Path})
	if d := cmp.Diff(wantPaths, paths); d != "" {
		t.Fatalf("targets (-want +got):\n%s", d)
	}
	for _, x := range tg {
		if x.Path == deadMounted.Path && (x.Kind != v1alpha1.MountKindPod || x.PodUID != "uid-dead") {
			t.Fatalf("orphan target %+v", x)
		}
	}
	dirs := DirsToInspect(in.Snapshot, in.PodVolumeDirs, in.GlobalmountDirs)
	wantDirs := []string{deadEmpty.Path, deadShadow.Path, liveOtherPV.Path}
	if d := cmp.Diff(sortedStrings(wantDirs), dirs); d != "" {
		t.Fatalf("DirsToInspect (-want +got):\n%s", d)
	}
}

func TestBuildUnknownGlobalmounts(t *testing.T) {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{smbPV("known", "known", "//s/k", "", "")}
	unknownListed := gmDir("ghost-listed")
	unknownMounted := gmDir("ghost-mounted")
	knownGM := gmDir("known")
	in.GlobalmountDirs = []hostfs.GlobalmountDir{unknownListed, knownGM, unknownMounted}
	in.Snapshot = hostfs.MountSnapshot{Entries: []hostfs.MountEntry{
		{MountPoint: "/", FSType: "xfs"},
		cifs(unknownMounted.Path),
		cifs(layout.StagingRoot() + "/onlymounted/globalmount"),
		cifs(layout.StagingRoot() + "/x/notglobalmount"),
		cifs(layout.StagingRoot() + "/a/b/globalmount"),
	}}
	in.DirFacts = map[string]hostfs.DirFacts{knownGM.Path: {Exists: true, IsDir: true, Empty: false}}
	got := Build(in)
	want := []hostfs.GlobalmountDir{unknownListed, unknownMounted, {Hash: "onlymounted", Path: layout.StagingRoot() + "/onlymounted/globalmount"}}
	sortGMs(want)
	if d := cmp.Diff(want, got.UnknownGlobalmounts); d != "" {
		t.Fatalf("unknown (-want +got):\n%s", d)
	}
	// Known gm: unmounted, not empty -> volume present with shadow data facts.
	if len(got.Volumes) != 1 || got.Volumes[0].Globalmount.Mounted || got.Volumes[0].Globalmount.Dir.Empty ||
		!got.Volumes[0].Globalmount.Dir.Exists {
		t.Fatalf("volumes %+v", got.Volumes)
	}
	// Unmounted gm is not a probe target; unknown gms are never targets.
	if tg := got.ProbeTargets(); len(tg) != 0 {
		t.Fatalf("targets %+v", tg)
	}
	dirs := DirsToInspect(in.Snapshot, in.PodVolumeDirs, in.GlobalmountDirs)
	if d := cmp.Diff(sortedStrings([]string{unknownListed.Path, knownGM.Path}), dirs); d != "" {
		t.Fatalf("DirsToInspect (-want +got):\n%s", d)
	}
}

func TestBuildListedGlobalmountWithoutFacts(t *testing.T) {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{smbPV("v", "v", "//s/v", "", "")}
	in.GlobalmountDirs = []hostfs.GlobalmountDir{gmDir("v")}
	got := Build(in)
	want := hostfs.DirFacts{Exists: true, IsDir: true, Empty: true}
	if len(got.Volumes) != 1 || got.Volumes[0].Globalmount.Dir != want {
		t.Fatalf("got %+v", got.Volumes)
	}
}

func TestBuildSkips(t *testing.T) {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{smbPV("v", "v", "//s/v", "ns", "c")}
	del := metav1.NewTime(now)
	young := metav1.NewTime(now.Add(-30 * time.Second))
	in.Pods = []corev1.Pod{
		pod("ns", "terminating", "u1", []string{"c"}, func(p *corev1.Pod) { p.DeletionTimestamp = &del }),
		pod("ns", "young-started", "u2", []string{"c"}, func(p *corev1.Pod) { p.Status.StartTime = &young }),
		pod("ns", "young-created", "u3", []string{"c"}, func(p *corev1.Pod) {
			p.Status.StartTime = nil
			p.CreationTimestamp = young
		}),
		pod("ns", "done", "u4", []string{"c"}, func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		pod("ns", "failed", "u5", []string{"c"}, func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }),
		pod("ns", "old", "u6", []string{"c", "c"}), // claim listed twice -> one mount
		pod("ns", "elsewhere", "u7", []string{"c"}, func(p *corev1.Pod) { p.Spec.NodeName = "other" }),
		pod("ns", "exactly-grace", "u8", []string{"c"}, func(p *corev1.Pod) {
			st := metav1.NewTime(now.Add(-grace))
			p.Status.StartTime = &st
		}),
	}
	got := Build(in)
	if len(got.Volumes) != 1 {
		t.Fatalf("volumes %+v", got.Volumes)
	}
	type sk struct {
		Skip        bool
		Reason      string
		Terminating bool
	}
	gotSkips := map[types.UID]sk{}
	for _, pm := range got.Volumes[0].PodMounts {
		gotSkips[pm.UID] = sk{pm.Skip, pm.SkipReason, pm.Terminating}
	}
	want := map[types.UID]sk{
		"u1": {true, SkipTerminating, true},
		"u2": {true, SkipStartupGrace, false},
		"u3": {true, SkipStartupGrace, false},
		"u4": {true, SkipPodFinished, false},
		"u5": {true, SkipPodFinished, false},
		"u6": {false, "", false},
		"u8": {false, "", false},
	}
	if d := cmp.Diff(want, gotSkips); d != "" {
		t.Fatalf("skips (-want +got):\n%s", d)
	}
	tg := got.ProbeTargets()
	if len(tg) != 2 {
		t.Fatalf("targets %+v", tg)
	}
}

func TestBuildWrongFSTypeSurfaced(t *testing.T) {
	// 2026-08-29: pod path is an xfs bind of the bare globalmount dir, the
	// globalmount itself is not mounted and holds shadow data.
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{smbPV("tx", "tx", "//s/tx", "dl", "tx")}
	in.Pods = []corev1.Pod{pod("dl", "transmission-0", "uid-t", []string{"tx"}, ownedBy("StatefulSet", "transmission"))}
	pp := layout.PodVolumeMountPath("uid-t", "tx")
	gm := layout.GlobalmountPath("tx")
	in.Snapshot = hostfs.MountSnapshot{Entries: []hostfs.MountEntry{
		cifs(pp),
		{MountPoint: pp, FSType: "xfs", Source: "/dev/vda6"}, // overmount: last wins
	}}
	in.GlobalmountDirs = []hostfs.GlobalmountDir{gmDir("tx")}
	in.PodVolumeDirs = []hostfs.PodVolumeDir{podDir("uid-t", "tx")}
	in.DirFacts = map[string]hostfs.DirFacts{gm: {Exists: true, IsDir: true}}
	got := Build(in)
	v := got.Volumes[0]
	if !v.PodMounts[0].Mounted || v.PodMounts[0].FSType != "xfs" {
		t.Fatalf("pod mount %+v", v.PodMounts[0])
	}
	if v.Globalmount.Mounted || v.Globalmount.Dir.Empty || !v.Globalmount.Dir.Exists {
		t.Fatalf("globalmount %+v", v.Globalmount)
	}
	if d := cmp.Diff([]string{gm}, DirsToInspect(in.Snapshot, in.PodVolumeDirs, in.GlobalmountDirs)); d != "" {
		t.Fatal(d)
	}
}

func TestWorkloadOf(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name   string
		refs   []metav1.OwnerReference
		labels map[string]string
		want   string
	}{
		{"no owner", nil, nil, ""},
		{"statefulset", []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", Controller: &yes}}, nil, "StatefulSet/db"},
		{"rs mapped to deployment", []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-5c8d", Controller: &yes}},
			map[string]string{"pod-template-hash": "5c8d"}, "Deployment/web"},
		{"rs without hash label", []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-5c8d"}}, nil, "ReplicaSet/web-5c8d"},
		{"rs hash mismatch", []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-5c8d"}},
			map[string]string{"pod-template-hash": "zzz"}, "ReplicaSet/web-5c8d"},
		{"rs name equals hash suffix only", []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "-h"}},
			map[string]string{"pod-template-hash": "h"}, "ReplicaSet/-h"},
		{"controller ref preferred", []metav1.OwnerReference{
			{Kind: "ConfigMap", Name: "x", Controller: &no},
			{Kind: "Job", Name: "backup", Controller: &yes},
		}, nil, "Job/backup"},
		{"first ref when no controller", []metav1.OwnerReference{{Kind: "Node", Name: "n1"}}, nil, "Node/n1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: tt.refs, Labels: tt.labels}}
			if got := workloadOf(p); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestBuildClaimEdgeCases(t *testing.T) {
	in := baseInput()
	dup1 := smbPV("b-pv", "b-pv", "//s/b", "ns", "shared")
	dup2 := smbPV("a-pv", "a-pv", "//s/a", "ns", "shared") // same claim: lowest name wins
	dupName := smbPV("a-pv", "zzz", "//s/zzz", "", "")     // duplicate PV name: first after sort wins
	eph := smbPV("eph", "eph", "//s/eph", "ns", "pod-e-scratch")
	in.PVs = []corev1.PersistentVolume{dup1, dup2, dupName, eph}
	p := pod("ns", "pod-e", "uid-e", []string{"shared"})
	p.Spec.Volumes = append(p.Spec.Volumes,
		corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}},
		corev1.Volume{Name: "cm", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
	)
	in.Pods = []corev1.Pod{p}
	got := Build(in)
	names := []string{}
	for _, v := range got.Volumes {
		names = append(names, v.PVName)
		if v.PVName == "a-pv" && v.VolumeHandle != "a-pv" {
			t.Errorf("duplicate-name PV resolved to handle %q", v.VolumeHandle)
		}
	}
	if d := cmp.Diff([]string{"a-pv", "eph"}, names); d != "" {
		t.Fatalf("volumes (-want +got):\n%s", d)
	}
}

// --- determinism ---

func richInput() Input {
	in := baseInput()
	in.PVs = []corev1.PersistentVolume{
		smbPV("cfg-a", "cfg-a", "//s/a", "app", "a"),
		smbPV("cfg-b", "cfg-b", "//s/b", "app", "b"),
		smbPV("shared", "shared", "//s/shared", "app", "shared"),
		smbPV("mirror", "mirror", "//s/shared", "other", "shared"),
		smbPV("unbound", "unbound", "//s/u", "", ""),
	}
	for i, n := range []string{"p1", "p2", "p3", "p4"} {
		claims := []string{"a", "shared"}
		if i%2 == 1 {
			claims = []string{"b", "shared"}
		}
		in.Pods = append(in.Pods, pod("app", n, "uid-"+n, claims, ownedBy("ReplicaSet", "x-"+n)))
	}
	in.Pods = append(in.Pods, pod("other", "o1", "uid-o1", []string{"shared"}))
	ents := []hostfs.MountEntry{}
	for _, h := range []string{"cfg-a", "shared", "mirror", "unbound", "ghost"} {
		ents = append(ents, cifs(layout.GlobalmountPath(h)))
		in.GlobalmountDirs = append(in.GlobalmountDirs, gmDir(h))
	}
	in.GlobalmountDirs = append(in.GlobalmountDirs, gmDir("cfg-b"), gmDir("ghost2"))
	for _, p := range in.Pods {
		for _, pv := range []string{"cfg-a", "shared"} {
			ents = append(ents, cifs(layout.PodVolumeMountPath(p.UID, pv)))
			in.PodVolumeDirs = append(in.PodVolumeDirs, podDir(string(p.UID), pv))
		}
	}
	for _, u := range []string{"dead1", "dead2", "dead3"} {
		in.PodVolumeDirs = append(in.PodVolumeDirs, podDir(u, "cfg-b"), podDir(u, "shared"))
		ents = append(ents, cifs(layout.PodVolumeMountPath(types.UID(u), "shared")))
	}
	in.Snapshot = hostfs.MountSnapshot{Entries: ents}
	for _, d := range DirsToInspect(in.Snapshot, in.PodVolumeDirs, in.GlobalmountDirs) {
		in.DirFacts[d] = hostfs.DirFacts{Exists: true, IsDir: true, Empty: len(d)%2 == 0}
	}
	return in
}

func shuffled(in Input, seed uint64) Input {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	out := in
	out.PVs = append([]corev1.PersistentVolume(nil), in.PVs...)
	out.Pods = append([]corev1.Pod(nil), in.Pods...)
	out.PodVolumeDirs = append([]hostfs.PodVolumeDir(nil), in.PodVolumeDirs...)
	out.GlobalmountDirs = append([]hostfs.GlobalmountDir(nil), in.GlobalmountDirs...)
	r.Shuffle(len(out.PVs), func(i, j int) { out.PVs[i], out.PVs[j] = out.PVs[j], out.PVs[i] })
	r.Shuffle(len(out.Pods), func(i, j int) { out.Pods[i], out.Pods[j] = out.Pods[j], out.Pods[i] })
	r.Shuffle(len(out.PodVolumeDirs), func(i, j int) {
		out.PodVolumeDirs[i], out.PodVolumeDirs[j] = out.PodVolumeDirs[j], out.PodVolumeDirs[i]
	})
	r.Shuffle(len(out.GlobalmountDirs), func(i, j int) {
		out.GlobalmountDirs[i], out.GlobalmountDirs[j] = out.GlobalmountDirs[j], out.GlobalmountDirs[i]
	})
	for i := range out.Pods {
		p := out.Pods[i].DeepCopy()
		r.Shuffle(len(p.Spec.Volumes), func(a, b int) { p.Spec.Volumes[a], p.Spec.Volumes[b] = p.Spec.Volumes[b], p.Spec.Volumes[a] })
		out.Pods[i] = *p
	}
	return out
}

func TestBuildRichSanity(t *testing.T) {
	v := Build(richInput())
	names := []string{}
	for _, vol := range v.Volumes {
		names = append(names, vol.PVName)
	}
	if d := cmp.Diff([]string{"cfg-a", "cfg-b", "mirror", "shared", "unbound"}, names); d != "" {
		t.Fatalf("volumes (-want +got):\n%s", d)
	}
	if len(v.UnknownGlobalmounts) != 2 {
		t.Fatalf("unknown %+v", v.UnknownGlobalmounts)
	}
}

func FuzzBuildDeterministic(f *testing.F) {
	base := richInput()
	want := Build(base)
	wantTargets := want.ProbeTargets()
	wantDirs := DirsToInspect(base.Snapshot, base.PodVolumeDirs, base.GlobalmountDirs)
	f.Add(uint64(1))
	f.Add(uint64(42))
	f.Fuzz(func(t *testing.T, seed uint64) {
		in := shuffled(base, seed)
		got := Build(in)
		if d := cmp.Diff(want, got); d != "" {
			t.Fatalf("Build depends on input order (-want +got):\n%s", d)
		}
		if d := cmp.Diff(wantTargets, got.ProbeTargets()); d != "" {
			t.Fatalf("ProbeTargets depend on input order:\n%s", d)
		}
		if d := cmp.Diff(wantDirs, DirsToInspect(in.Snapshot, in.PodVolumeDirs, in.GlobalmountDirs)); d != "" {
			t.Fatalf("DirsToInspect depends on input order:\n%s", d)
		}
	})
}

// --- helpers ---

func sortedStrings(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func sortedOrphans(o ...OrphanDirView) []OrphanDirView {
	slices.SortFunc(o, func(a, b OrphanDirView) int { return strings.Compare(a.Path, b.Path) })
	return o
}

func sortGMs(g []hostfs.GlobalmountDir) {
	slices.SortFunc(g, func(a, b hostfs.GlobalmountDir) int { return strings.Compare(a.Hash, b.Hash) })
}
