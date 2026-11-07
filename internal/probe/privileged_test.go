//go:build privileged

package probe

import (
	"context"
	"testing"
	"time"

	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
)

// Run as root: go test -tags privileged ./internal/probe/...
func TestPrivilegedRealUmount(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "size=1m"); err != nil {
		t.Skipf("mount tmpfs: %v (needs CAP_SYS_ADMIN)", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(dir, unix.MNT_DETACH) })
	r := newTestRunner(t, 10*time.Second)
	if err := r.Umount(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if ok, err := mountinfo.Mounted(dir); err != nil || ok {
		t.Fatalf("still mounted: %v %v", ok, err)
	}
	if err := r.Umount(context.Background(), dir); err == nil {
		t.Fatal("second umount of a plain dir succeeded")
	}
}
