package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

func TestHost(t *testing.T) {
	ctx := context.Background()
	layout := kubelet.Layout{Root: "/var/lib/kubelet", DriverName: "smb.csi.k8s.io"}
	h := NewHost(layout, "/var/lib/kubelet/q", "cifs", "smb3")
	gm := layout.GlobalmountPath("vh")
	pod := layout.PodVolumeMountPath("u1", "pv")
	h.Dirs.SetDir(gm, Dir{Entries: 2, Files: 3, Bytes: 10})
	h.Dirs.SetDir(pod, Dir{})
	h.Mounts.Mount(pod, "cifs", "//srv", "soft")
	h.Mounts.Mount(pod, "xfs", "/dev/vda6", "") // overmount

	snap, _ := h.Mounts.Snapshot(ctx)
	if r := h.Prober.Probe(ctx, pod, snap); r.Class != hostfs.ProbeWrongFSType {
		t.Fatalf("overmount probe: %+v", r)
	}
	if r := h.Prober.Probe(ctx, gm, snap); r.Class != hostfs.ProbeNotMountpoint {
		t.Fatalf("gm probe: %+v", r)
	}
	if _, err := h.Dirs.Inspect(ctx, pod); !errors.Is(err, hostfs.ErrIsMountpoint) {
		t.Fatalf("inspect mounted: %v", err)
	}
	if pods, _ := h.Dirs.ListPodVolumeDirs(ctx); len(pods) != 1 || pods[0].PodUID != "u1" || pods[0].PVName != "pv" {
		t.Fatalf("pods: %+v", pods)
	}
	if gms, _ := h.Dirs.ListGlobalmountDirs(ctx); len(gms) != 1 || gms[0].Hash != kubelet.GlobalmountHash("vh") {
		t.Fatalf("gms: %+v", gms)
	}

	// Unmount the overmount: the cifs mount below becomes visible.
	if err := h.Mutator.ForceUnmount(ctx, pod); err != nil {
		t.Fatal(err)
	}
	snap, _ = h.Mounts.Snapshot(ctx)
	h.Prober.Script(pod, hostfs.ProbeStatTimeout)
	if r := h.Prober.Probe(ctx, pod, snap); r.Class != hostfs.ProbeStatTimeout {
		t.Fatalf("scripted: %+v", r)
	}
	if r := h.Prober.Probe(ctx, pod, snap); r.Class != hostfs.ProbeOK {
		t.Fatalf("after script: %+v", r)
	}
	h.Prober.SetHung(pod, true)
	if r := h.Prober.Probe(ctx, pod, snap); r.Class != hostfs.ProbeHung || h.Prober.Hung() != 1 {
		t.Fatalf("hung: %+v", r)
	}
	if h.Prober.Calls(pod) != 4 {
		t.Fatalf("calls = %d", h.Prober.Calls(pod))
	}

	// Quarantine: refuses mountpoints; moves aggregates; collision suffix.
	if _, err := h.Mutator.Quarantine(ctx, pod, "/var/lib/kubelet/q/pv-20261011T000000Z"); !errors.Is(err, hostfs.ErrIsMountpoint) {
		t.Fatalf("mounted quarantine: %v", err)
	}
	h.Quarantine.SetFolder("pv-20261011T000000Z", hostfs.QuarantineStats{Entries: 1})
	mv, err := h.Mutator.Quarantine(ctx, gm, "/var/lib/kubelet/q/pv-20261011T000000Z")
	if err != nil || mv.Destination != "/var/lib/kubelet/q/pv-20261011T000000Z-2" || mv.Entries != 2 {
		t.Fatalf("quarantine: %+v %v", mv, err)
	}
	if f, _ := h.Dirs.Inspect(ctx, gm); !f.Empty {
		t.Fatalf("source not emptied: %+v", f)
	}
	st, err := h.Quarantine.Scan(ctx, mv.Destination)
	if err != nil || st.Files != 3 || st.Bytes != 10 {
		t.Fatalf("scan: %+v %v", st, err)
	}
	if mv, err := h.Mutator.Quarantine(ctx, gm, "/var/lib/kubelet/q/x"); err != nil || mv.Destination != "" {
		t.Fatalf("empty source: %+v %v", mv, err)
	}
	if !h.Dirs.RemoveDir(gm) {
		t.Fatal("empty dir not removable")
	}
	h.Quarantine.RemoveFolder("pv-20261011T000000Z-2")
	if fs, _ := h.Quarantine.ListFolders(ctx); len(fs) != 1 {
		t.Fatalf("folders: %+v", fs)
	}
	if len(h.Mutator.Calls()) != 4 {
		t.Fatalf("calls: %+v", h.Mutator.Calls())
	}
}
