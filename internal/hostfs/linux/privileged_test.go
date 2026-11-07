//go:build privileged

package linux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// Run as root: go test -tags privileged ./internal/hostfs/linux/...
func TestPrivilegedRealMountpoint(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "gm")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", src, "tmpfs", 0, "size=1m"); err != nil {
		t.Skipf("mount tmpfs: %v (needs CAP_SYS_ADMIN)", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(src, unix.MNT_DETACH) })
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mounts := NewMountTable(root)
	m := &Mutator{Root: root, Mounts: mounts}
	if _, err := m.Quarantine(context.Background(), src, filepath.Join(root, "q", "pv-20261011T000000Z")); !errors.Is(err, hostfs.ErrIsMountpoint) {
		t.Fatalf("quarantine of a live tmpfs mount: %v", err)
	}
	d := DirInspector{Layout: kubelet.Layout{Root: root, DriverName: "x"}, Mounts: mounts}
	if _, err := d.Inspect(context.Background(), src); !errors.Is(err, hostfs.ErrIsMountpoint) {
		t.Fatalf("inspect: %v", err)
	}
	m.Unmounter = unmountFunc(func(_ context.Context, p string) error { return unix.Unmount(p, unix.MNT_FORCE|unix.MNT_DETACH) })
	if err := m.ForceUnmount(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if f, err := d.Inspect(context.Background(), src); err != nil || !f.Empty {
		t.Fatalf("after umount: %+v %v", f, err)
	}
}
