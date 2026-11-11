package linux

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

const fixture = "testdata/mountinfo-2026-08-29-xfs-overmount"

func TestMountTableFixture(t *testing.T) {
	snap, err := MountTable{Prefix: "/var/lib/kubelet", Path: fixture}.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, e := range snap.Entries {
		ids = append(ids, e.ID)
	}
	// Prefix filter keeps the kubelet root and below (not /var/lib/kubelet-other), in file order.
	if got, want := fmt.Sprint(ids), "[30 40 41 50 51 60 61 70]"; got != want {
		t.Fatalf("ids = %s, want %s", got, want)
	}

	gm := "/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/1e8c0e6f6a1d2b0f1b6e0a2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5a6/globalmount"
	e, ok := snap.Lookup(gm)
	if !ok || e.FSType != "cifs" || e.Source != "//u123.your-storagebox.de/backup/library" ||
		!strings.Contains(","+e.SuperOptions+",", ",soft,") || e.Options != "rw,relatime" || e.ParentID != 30 {
		t.Fatalf("cifs globalmount: %+v", e)
	}

	// 2026-08-29: the pod path is a bind of the bare globalmount dir on /dev/vda6 (xfs).
	pod := "/var/lib/kubelet/pods/d2898609-aaaa-4bbb-8ccc-dddddddddddd/volumes/kubernetes.io~csi/storagebox-transmission-config/mount"
	e, ok = snap.Lookup(pod)
	if !ok || e.FSType != "xfs" || e.Source != "/dev/vda6" || !strings.HasSuffix(e.Root, "/globalmount") {
		t.Fatalf("xfs bind: %+v", e)
	}

	// Overmount: cifs then xfs on the same path; the last entry is visible.
	over := "/var/lib/kubelet/pods/e1e1e1e1-0000-4000-8000-000000000000/volumes/kubernetes.io~csi/overmounted/mount"
	if e, _ := snap.Lookup(over); e.ID != 61 || e.FSType != "xfs" {
		t.Fatalf("overmount: %+v", e)
	}
	// Octal escapes are decoded.
	if !snap.IsMountpoint("/var/lib/kubelet/pods/f0f0/volumes/kubernetes.io~empty-dir/with space") {
		t.Fatal("escaped mountpoint not decoded")
	}

	all, err := MountTable{Prefix: "/", Path: fixture}.Snapshot(context.Background())
	if err != nil || len(all.Entries) != 12 || all.Entries[0].MountPoint != "/" {
		t.Fatalf("prefix / : %d entries, %v", len(all.Entries), err)
	}
}

func TestMountTableLive(t *testing.T) {
	snap, err := NewMountTable("/").Snapshot(context.Background())
	if err != nil || !snap.IsMountpoint("/") {
		t.Fatalf("live mountinfo: %v", err)
	}
}

// --- DirInspector ---

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirs(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lockDir makes dir unreadable for a non-root user, so any readdir/open of
// it by the code under test fails: proof that it is never touched.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func TestDirInspector(t *testing.T) {
	root := t.TempDir()
	layout := kubelet.Layout{Root: root, DriverName: "smb.csi.k8s.io"}
	mounts := &fake.MountTable{}
	d := DirInspector{Layout: layout, Mounts: mounts}

	mountedPod := layout.PodVolumeMountPath("uid-1", "pv-a")
	orphan := layout.PodVolumeMountPath("uid-2", "pv-a")
	emptyPod := layout.PodVolumeMountPath("uid-2", "pv-b")
	gmMounted := layout.GlobalmountPath("vh-a")
	gmShadow := layout.GlobalmountPath("vh-b")
	mkdirs(t, mountedPod, emptyPod, gmMounted,
		filepath.Join(layout.PodsRoot(), "uid-3", "volumes", "kubernetes.io~csi", "pv-c"), // no mount dir
		filepath.Join(layout.StagingRoot(), "deadbeef"),                                   // no globalmount
	)
	writeFile(t, filepath.Join(orphan, ".shadow"), "x")
	writeFile(t, filepath.Join(gmShadow, "old.txt"), "y")
	writeFile(t, filepath.Join(layout.PodsRoot(), "not-a-dir"), "")
	mounts.Mount(mountedPod, "cifs", "//srv/a", "soft")
	mounts.Mount(gmMounted, "cifs", "//srv/a", "soft")
	lockDir(t, mountedPod)
	lockDir(t, gmMounted)
	ctx := context.Background()

	pods, err := d.ListPodVolumeDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pods); got != fmt.Sprint([]hostfs.PodVolumeDir{
		{PodUID: "uid-1", PVName: "pv-a", Path: mountedPod},
		{PodUID: "uid-2", PVName: "pv-a", Path: orphan},
		{PodUID: "uid-2", PVName: "pv-b", Path: emptyPod},
	}) {
		t.Fatalf("pods = %s", got)
	}
	gms, err := d.ListGlobalmountDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gms) != 2 {
		t.Fatalf("gms = %+v", gms)
	}
	for _, g := range gms {
		if g.Path != filepath.Join(layout.StagingRoot(), g.Hash, "globalmount") {
			t.Fatalf("gm = %+v", g)
		}
	}

	tests := []struct {
		path    string
		want    hostfs.DirFacts
		wantErr error
	}{
		{path: mountedPod, wantErr: hostfs.ErrIsMountpoint},
		{path: gmMounted, wantErr: hostfs.ErrIsMountpoint},
		{path: orphan, want: hostfs.DirFacts{Exists: true, IsDir: true}},
		{path: gmShadow, want: hostfs.DirFacts{Exists: true, IsDir: true}},
		{path: emptyPod, want: hostfs.DirFacts{Exists: true, IsDir: true, Empty: true}},
		{path: filepath.Join(layout.PodsRoot(), "not-a-dir"), want: hostfs.DirFacts{Exists: true}},
		{path: layout.PodVolumeMountPath("uid-9", "pv-z"), want: hostfs.DirFacts{}},
	}
	for _, tt := range tests {
		got, err := d.Inspect(ctx, tt.path)
		if !errors.Is(err, tt.wantErr) || got != tt.want {
			t.Fatalf("Inspect(%s) = %+v, %v; want %+v, %v", tt.path, got, err, tt.want, tt.wantErr)
		}
	}

	// A mountpoint parent below the root is never read.
	parentMounted := filepath.Join(root, "pods", "uid-1", "volumes", "kubernetes.io~csi", "pv-a")
	mounts.Mount(parentMounted, "cifs", "//srv/x", "")
	if _, err := d.Inspect(ctx, filepath.Join(parentMounted, "other")); !errors.Is(err, errParentMounted) {
		t.Fatalf("parent mounted: %v", err)
	}
	if pods, err := d.ListPodVolumeDirs(ctx); err != nil || len(pods) != 2 {
		t.Fatalf("mounted pv dir must be skipped: %+v %v", pods, err)
	}
}

// TestDirInspectorVanishesAfterListing: kubelet removes a pod volume dir
// between the parent listing and the read of the dir itself (S5 in e2e:
// quarantine failed with "read directory: no such file or directory").
// Inspect must report it as missing, not fail.
func TestDirInspectorVanishesAfterListing(t *testing.T) {
	root := t.TempDir()
	layout := kubelet.Layout{Root: root, DriverName: "smb.csi.k8s.io"}
	d := DirInspector{Layout: layout, Mounts: &fake.MountTable{}}
	path := layout.PodVolumeMountPath("uid-1", "pv-a")
	mkdirs(t, path)

	testHookBeforeRead = func(p string) {
		if p == path {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { testHookBeforeRead = nil })

	got, err := d.Inspect(context.Background(), path)
	if err != nil || got != (hostfs.DirFacts{}) {
		t.Fatalf("Inspect = %+v, %v; want missing, no error", got, err)
	}
}

func TestDirInspectorMissingRoot(t *testing.T) {
	d := DirInspector{Layout: kubelet.Layout{Root: filepath.Join(t.TempDir(), "nope"), DriverName: "x"}, Mounts: &fake.MountTable{}}
	pods, err1 := d.ListPodVolumeDirs(context.Background())
	gms, err2 := d.ListGlobalmountDirs(context.Background())
	if pods != nil || gms != nil || err1 != nil || err2 != nil {
		t.Fatalf("%v %v %v %v", pods, gms, err1, err2)
	}
}

// --- Mutator ---

const secret = "SECRET-tax-return-2026.pdf"

type fakeUnmounter struct {
	mounts *fake.MountTable
	calls  []string
	err    error
}

func (u *fakeUnmounter) Umount(_ context.Context, path string) error {
	u.calls = append(u.calls, path)
	if u.err != nil {
		return u.err
	}
	u.mounts.Unmount(path)
	return nil
}

func newMutator(t *testing.T) (*Mutator, *fake.MountTable, string) {
	t.Helper()
	root := t.TempDir()
	mounts := &fake.MountTable{}
	return &Mutator{Root: root, Mounts: mounts, Unmounter: &fakeUnmounter{mounts: mounts}}, mounts, root
}

func seedShadow(t *testing.T, src string) {
	t.Helper()
	writeFile(t, filepath.Join(src, secret), "payload")
	writeFile(t, filepath.Join(src, ".hidden"), "dot")
	writeFile(t, filepath.Join(src, "dir", "nested", "deep.bin"), "deep")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
}

func assertMoved(t *testing.T, dest string) {
	t.Helper()
	for name, want := range map[string]string{secret: "payload", ".hidden": "dot", "dir/nested/deep.bin": "deep"} {
		b, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(dest, "link")); err != nil || target != "/etc/passwd" {
		t.Fatalf("symlink: %q %v", target, err)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 0 {
		t.Fatalf("%s not empty: %v %v", dir, ents, err)
	}
}

func TestQuarantineMovesEverythingIncludingDotfiles(t *testing.T) {
	m, _, root := newMutator(t)
	src := filepath.Join(root, "plugins", "gm")
	seedShadow(t, src)
	qdir := filepath.Join(root, "quarantine")
	dest := filepath.Join(qdir, kubelet.QuarantineFolderName("pv-a", time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)))

	mv, err := m.Quarantine(context.Background(), src, dest)
	if err != nil || mv.Destination != dest || mv.Entries != 4 {
		t.Fatalf("move = %+v, %v", mv, err)
	}
	assertMoved(t, dest)
	assertEmptyDir(t, src) // src itself stays (kubelet removes it)
	if fi, err := os.Stat(dest); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dest mode: %v %v", fi, err)
	}

	// Idempotent: an empty source moves and creates nothing.
	mv, err = m.Quarantine(context.Background(), src, filepath.Join(qdir, "pv-a-20260829T010204Z"))
	if err != nil || mv != (hostfs.QuarantineMove{}) {
		t.Fatalf("second = %+v, %v", mv, err)
	}
	if _, err := os.Stat(filepath.Join(qdir, "pv-a-20260829T010204Z")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty source created a folder: %v", err)
	}
	// Missing source: nothing to do.
	if mv, err := m.Quarantine(context.Background(), filepath.Join(root, "gone"), dest); err != nil || mv.Destination != "" {
		t.Fatalf("missing = %+v, %v", mv, err)
	}
}

func TestQuarantineCollisionSuffix(t *testing.T) {
	m, _, root := newMutator(t)
	qdir := filepath.Join(root, "quarantine")
	ts := time.Date(2026, 10, 11, 7, 8, 9, 0, time.UTC)
	dest := filepath.Join(qdir, kubelet.QuarantineFolderName("pv-a", ts))
	mkdirs(t, dest, dest+"-2")
	src := filepath.Join(root, "pods", "u", "mount")
	writeFile(t, filepath.Join(src, "f"), "1")

	mv, err := m.Quarantine(context.Background(), src, dest)
	if err != nil || mv.Destination != dest+"-3" || mv.Entries != 1 {
		t.Fatalf("move = %+v, %v", mv, err)
	}
	if mv.Destination != filepath.Join(qdir, kubelet.QuarantineFolderNameN("pv-a", ts, 3)) {
		t.Fatalf("suffix format differs from kubelet.QuarantineFolderNameN: %s", mv.Destination)
	}
	if pv, got, ok := kubelet.ParseQuarantineFolderName(filepath.Base(mv.Destination)); !ok || pv != "pv-a" || !got.Equal(ts) {
		t.Fatalf("parse %s: %q %v %v", mv.Destination, pv, got, ok)
	}
}

func TestQuarantineRefusesLiveMountpoint(t *testing.T) {
	m, mounts, root := newMutator(t)
	src := filepath.Join(root, "gm")
	seedShadow(t, src)
	dest := filepath.Join(root, "q", "pv-20261011T000000Z")

	for _, mp := range []string{src, filepath.Join(src, "dir")} {
		mounts.Set()
		mounts.Mount(mp, "cifs", "//srv/x", "soft")
		mv, err := m.Quarantine(context.Background(), src, dest)
		if !errors.Is(err, hostfs.ErrIsMountpoint) || mv != (hostfs.QuarantineMove{}) {
			t.Fatalf("mount at %s: %+v, %v", mp, mv, err)
		}
	}
	if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("refused quarantine created %s", dest)
	}
	if _, err := os.Stat(filepath.Join(src, secret)); err != nil {
		t.Fatal("refused quarantine moved data")
	}
	// Outside the root or into itself: refused.
	if _, err := m.Quarantine(context.Background(), "/etc", dest); err == nil {
		t.Fatal("quarantine outside root accepted")
	}
	mounts.Set()
	if _, err := m.Quarantine(context.Background(), src, filepath.Join(src, "q")); err == nil {
		t.Fatal("dest inside src accepted")
	}
}

func TestQuarantineEXDEVFallback(t *testing.T) {
	m, _, root := newMutator(t)
	src := filepath.Join(root, "gm")
	seedShadow(t, src)
	old := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, secret), old, old); err != nil {
		t.Fatal(err)
	}
	var renames int
	m.Rename = func(oldpath, newpath string) error {
		renames++
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: unix.EXDEV}
	}
	dest := filepath.Join(root, "q", "pv-20261011T000000Z")
	mv, err := m.Quarantine(context.Background(), src, dest)
	if err != nil || mv.Entries != 4 || renames != 4 {
		t.Fatalf("move = %+v, %v (renames %d)", mv, err, renames)
	}
	assertMoved(t, dest)
	assertEmptyDir(t, src)
	if fi, err := os.Stat(filepath.Join(dest, secret)); err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("mtime not preserved: %v %v", fi, err)
	}
}

func TestQuarantineErrorsNeverNameEntries(t *testing.T) {
	m, _, root := newMutator(t)
	src := filepath.Join(root, "gm")
	seedShadow(t, src)
	writeFile(t, filepath.Join(src, "fifo-dir", "x"), "")
	if err := unix.Mkfifo(filepath.Join(src, "fifo-dir", secret+".fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Rename = func(oldpath, newpath string) error {
		if filepath.Base(oldpath) == "fifo-dir" {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: unix.EXDEV} // copy hits the fifo
		}
		if filepath.Base(oldpath) == secret {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: unix.EACCES}
		}
		return os.Rename(oldpath, newpath)
	}
	mv, err := m.Quarantine(context.Background(), src, filepath.Join(root, "q", "pv-20261011T000000Z"))
	if err == nil || mv.Entries != 3 {
		t.Fatalf("move = %+v, %v", mv, err)
	}
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, errUnsupportedType) {
		t.Fatalf("errno lost: %v", err)
	}
	for _, s := range []string{err.Error(), fmt.Sprintf("%+v", mv)} {
		if strings.Contains(s, "SECRET") || strings.Contains(s, "fifo-dir") || strings.Contains(s, "hidden") {
			t.Fatalf("entry name leaked: %s", s)
		}
	}
	// Failed entries stay in the source, untouched.
	if _, err := os.Stat(filepath.Join(src, secret)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "fifo-dir", "x")); err != nil {
		t.Fatal("failed copy removed the source")
	}
}

func TestForceUnmount(t *testing.T) {
	m, mounts, root := newMutator(t)
	u := m.Unmounter.(*fakeUnmounter)
	p := filepath.Join(root, "gm")
	if err := m.ForceUnmount(context.Background(), p); err != nil || len(u.calls) != 0 {
		t.Fatalf("not mounted: %v, calls %v", err, u.calls)
	}
	mounts.Mount(p, "cifs", "//srv", "")
	mounts.Mount(p, "xfs", "/dev/vda6", "") // overmount: one call detaches the top only
	if err := m.ForceUnmount(context.Background(), p); err != nil || len(u.calls) != 1 || !mounts.IsMountpoint(p) {
		t.Fatalf("first: %v %v", err, u.calls)
	}
	u.err = fmt.Errorf("umount: %w", unix.EPERM)
	if err := m.ForceUnmount(context.Background(), p); !errors.Is(err, unix.EPERM) {
		t.Fatalf("eperm: %v", err)
	}
	// EINVAL because someone else unmounted it meanwhile: success.
	u.err = fmt.Errorf("umount: %w", unix.EINVAL)
	m.Unmounter = unmountFunc(func(_ context.Context, path string) error { mounts.Unmount(path); return u.err })
	if err := m.ForceUnmount(context.Background(), p); err != nil {
		t.Fatalf("einval race: %v", err)
	}
	// ENOENT because kubelet unmounted and removed the directory meanwhile
	// (a drained pod's volume path; seen in e2e S4): success.
	mounts.Mount(p, "xfs", "/dev/vda6", "")
	u.err = fmt.Errorf("umount: %w", unix.ENOENT)
	if err := m.ForceUnmount(context.Background(), p); err != nil {
		t.Fatalf("enoent race: %v", err)
	}
	// ENOENT while the path is still a mountpoint is a real error.
	mounts.Mount(p, "xfs", "/dev/vda6", "")
	m.Unmounter = unmountFunc(func(context.Context, string) error { return fmt.Errorf("umount: %w", unix.ENOENT) })
	if err := m.ForceUnmount(context.Background(), p); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("enoent while mounted: %v", err)
	}
	if err := m.ForceUnmount(context.Background(), "/proc"); err == nil {
		t.Fatal("path outside root accepted")
	}
}

type unmountFunc func(ctx context.Context, path string) error

func (f unmountFunc) Umount(ctx context.Context, path string) error { return f(ctx, path) }

// --- QuarantineInspector ---

func TestQuarantineInspectorList(t *testing.T) {
	qdir := filepath.Join(t.TempDir(), "q")
	q := QuarantineInspector{Dir: qdir, MaxEntries: 100, TimeBudget: time.Second}
	if got, err := q.ListFolders(context.Background()); got != nil || err != nil {
		t.Fatalf("missing root: %v %v", got, err)
	}
	mkdirs(t, filepath.Join(qdir, "pv-b-20261011T000000Z"), filepath.Join(qdir, "pv-a-20260829T000000Z"))
	writeFile(t, filepath.Join(qdir, "stray-file"), "")
	if err := os.Symlink("/etc", filepath.Join(qdir, "link-folder")); err != nil {
		t.Fatal(err)
	}
	got, err := q.ListFolders(context.Background())
	if err != nil || len(got) != 2 || got[0].Name != "pv-a-20260829T000000Z" || got[0].Path != filepath.Join(qdir, got[0].Name) {
		t.Fatalf("folders = %+v, %v", got, err)
	}
}

func seedFolder(t *testing.T) (QuarantineInspector, string) {
	t.Helper()
	qdir := t.TempDir()
	folder := filepath.Join(qdir, "pv-a-20260829T000000Z")
	old := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	writeFile(t, filepath.Join(folder, secret), "12345")
	writeFile(t, filepath.Join(folder, ".SECRET-dot"), "1")
	writeFile(t, filepath.Join(folder, "SECRET-dir", "a", "b.txt"), "1234567890")
	for _, p := range []string{secret, ".SECRET-dot", "SECRET-dir/a/b.txt", "SECRET-dir/a", "SECRET-dir"} {
		ts := newer
		if p == secret {
			ts = old
		}
		if err := os.Chtimes(filepath.Join(folder, p), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink to a big directory outside: counted, never followed.
	outside := t.TempDir()
	for i := range 50 {
		writeFile(t, filepath.Join(outside, fmt.Sprintf("f%d", i)), "xxxxxxxxxx")
	}
	if err := os.Symlink(outside, filepath.Join(folder, "SECRET-link")); err != nil {
		t.Fatal(err)
	}
	return QuarantineInspector{Dir: qdir, MaxEntries: 1000, TimeBudget: time.Minute}, folder
}

func assertNoNames(t *testing.T, v ...any) {
	t.Helper()
	for _, x := range v {
		if s := fmt.Sprintf("%+v", x); strings.Contains(s, "SECRET") {
			t.Fatalf("entry name leaked: %s", s)
		}
	}
}

func TestQuarantineScan(t *testing.T) {
	q, folder := seedFolder(t)
	st, err := q.Scan(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	assertNoNames(t, st)
	// top-level: secret, .SECRET-dot, SECRET-dir, SECRET-link
	if st.Entries != 4 || st.Files != 3 || st.Bytes != 16 || st.SizeTruncated {
		t.Fatalf("stats = %+v", st)
	}
	if !st.OldestModTime.Equal(time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)) || st.NewestModTime.Before(time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("mod times = %v .. %v", st.OldestModTime, st.NewestModTime)
	}
	// Folder must be directly under Dir.
	if _, err := q.Scan(context.Background(), filepath.Join(folder, "SECRET-dir")); err == nil {
		t.Fatal("scan of a nested dir accepted")
	}
	if _, err := q.Scan(context.Background(), filepath.Join(q.Dir, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing folder: %v", err)
	}
}

func TestQuarantineScanEntryBudget(t *testing.T) {
	q, folder := seedFolder(t)
	q.MaxEntries = 3
	st, err := q.Scan(context.Background(), folder)
	if err != nil || !st.SizeTruncated || st.Entries > 3 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	assertNoNames(t, st)
}

func TestQuarantineScanTimeBudget(t *testing.T) {
	q, folder := seedFolder(t)
	base := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	calls := 0
	q.Now = func() time.Time { calls++; return base.Add(time.Duration(calls) * time.Second) }
	q.TimeBudget = 2500 * time.Millisecond // start=+1s, deadline=+3.5s: two entries fit
	st, err := q.Scan(context.Background(), folder)
	if err != nil || !st.SizeTruncated || st.Entries != 2 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
}

func TestQuarantineScanUnreadableAndMounted(t *testing.T) {
	q, folder := seedFolder(t)
	if os.Getuid() != 0 {
		lockDir(t, filepath.Join(folder, "SECRET-dir"))
		st, err := q.Scan(context.Background(), folder)
		if err != nil || !st.SizeTruncated || st.Entries != 4 {
			t.Fatalf("unreadable subdir: %+v, %v", st, err)
		}
		assertNoNames(t, st, err)
		_ = os.Chmod(filepath.Join(folder, "SECRET-dir"), 0o755)
	}
	mounts := &fake.MountTable{}
	mounts.Mount(filepath.Join(folder, "SECRET-dir"), "cifs", "//srv", "")
	q.Mounts = mounts
	st, err := q.Scan(context.Background(), folder)
	if err != nil || !st.SizeTruncated || st.Files != 2 {
		t.Fatalf("mounted subdir: %+v, %v", st, err)
	}
	mounts.Mount(folder, "cifs", "//srv", "")
	if _, err := q.Scan(context.Background(), folder); !errors.Is(err, hostfs.ErrIsMountpoint) {
		t.Fatalf("mounted folder: %v", err)
	}
}

func TestSanitize(t *testing.T) {
	err := sanitize("rename", &os.LinkError{Op: "rename", Old: "/a/" + secret, New: "/b/" + secret, Err: unix.ENOSPC})
	if !errors.Is(err, unix.ENOSPC) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
	err = sanitize("x", fmt.Errorf("weird %s", secret))
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
}
