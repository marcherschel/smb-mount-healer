package kubelet

import (
	"testing"
	"time"
)

func TestPaths(t *testing.T) {
	l := Layout{Root: "/var/lib/kubelet", DriverName: "smb.csi.k8s.io"}
	// sha256("storagebox-x")
	if got, want := l.GlobalmountPath("storagebox-x"), "/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/"+GlobalmountHash("storagebox-x")+"/globalmount"; got != want {
		t.Fatalf("GlobalmountPath = %q, want %q", got, want)
	}
	if len(GlobalmountHash("a")) != 64 {
		t.Fatal("hash must be 64 hex chars")
	}
	if got, want := l.PodVolumeMountPath("u-1", "pv"), "/var/lib/kubelet/pods/u-1/volumes/kubernetes.io~csi/pv/mount"; got != want {
		t.Fatalf("PodVolumeMountPath = %q, want %q", got, want)
	}
}

func TestQuarantineFolderNameRoundTrip(t *testing.T) {
	ts := time.Date(2026, 10, 11, 7, 8, 9, 0, time.UTC)
	name := QuarantineFolderName("storagebox-transmission-config", ts)
	if name != "storagebox-transmission-config-20261011T070809Z" {
		t.Fatalf("name = %q", name)
	}
	pv, got, ok := ParseQuarantineFolderName(name)
	if !ok || pv != "storagebox-transmission-config" || !got.Equal(ts) {
		t.Fatalf("parse = %q %v %v", pv, got, ok)
	}
	if _, _, ok := ParseQuarantineFolderName("random-dir"); ok {
		t.Fatal("parsed a non-quarantine name")
	}
}

func TestQuarantineFolderNameCollisionSuffix(t *testing.T) {
	ts := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	if got := QuarantineFolderNameN("pv-a", ts, 1); got != "pv-a-20260829T010203Z" {
		t.Fatalf("n=1: %q", got)
	}
	name := QuarantineFolderNameN("pv-a", ts, 3)
	if name != "pv-a-20260829T010203Z-3" {
		t.Fatalf("n=3: %q", name)
	}
	pv, got, ok := ParseQuarantineFolderName(name)
	if !ok || pv != "pv-a" || !got.Equal(ts) {
		t.Fatalf("parse = %q %v %v", pv, got, ok)
	}
	for _, bad := range []string{"pv-a-20260829T010203Z-0", "pv-a-20260829T010203Z-x", "pv-a-20260829T010203Z-"} {
		if _, _, ok := ParseQuarantineFolderName(bad); ok {
			t.Fatalf("parsed %q", bad)
		}
	}
}
