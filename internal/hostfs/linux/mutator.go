package linux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// Unmounter runs umount2(MNT_FORCE|MNT_DETACH) out of process.
// *probe.Runner implements it.
type Unmounter interface {
	Umount(ctx context.Context, path string) error
}

// maxCollisions bounds the "-<n>" suffixes tried for a quarantine folder.
const maxCollisions = 1000

// quarantineDirMode: quarantined data may be sensitive.
const quarantineDirMode = 0o700

var errUnsupportedType = errors.New("unsupported file type")

// Mutator implements hostfs.Mutator. Both operations only act on paths
// strictly below Root, the prefix Mounts is filtered on.
type Mutator struct {
	Root      string
	Mounts    hostfs.MountTable
	Unmounter Unmounter
	// Rename defaults to os.Rename (test hook for EXDEV).
	Rename func(oldpath, newpath string) error
}

var _ hostfs.Mutator = (*Mutator)(nil)

func (m *Mutator) checkPath(op, path string) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || !strictlyBelow(clean, filepath.Clean(m.Root)) {
		return "", fmt.Errorf("%s %s: path must be strictly below %s", op, path, m.Root)
	}
	return clean, nil
}

// ForceUnmount implements hostfs.Mutator. A path that is not a mountpoint is
// already unmounted (nil). One call detaches the topmost mount only.
func (m *Mutator) ForceUnmount(ctx context.Context, path string) error {
	path, err := m.checkPath("force-unmount", path)
	if err != nil {
		return err
	}
	snap, err := m.Mounts.Snapshot(ctx)
	if err != nil {
		return err
	}
	if !snap.IsMountpoint(path) {
		return nil
	}
	if err := m.Unmounter.Umount(ctx, path); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			// Raced with another unmount? (EINVAL: no longer a mountpoint;
			// ENOENT: kubelet unmounted and removed the directory, e.g. a
			// drained pod's volume path.)
			if snap, serr := m.Mounts.Snapshot(ctx); serr == nil && !snap.IsMountpoint(path) {
				return nil
			}
		}
		return fmt.Errorf("force-unmount %s: %w", path, err)
	}
	return nil
}

// Quarantine implements hostfs.Mutator. It refuses (ErrIsMountpoint) when
// srcDir or anything below it is a mountpoint, checked on a fresh snapshot
// before listing and again right before the first move. destDir is created
// exclusively; if it exists, "<destDir>-2", "-3", ... are tried
// (kubelet.QuarantineFolderNameN). Errors never name an entry of srcDir.
func (m *Mutator) Quarantine(ctx context.Context, srcDir, destDir string) (hostfs.QuarantineMove, error) {
	src, err := m.checkPath("quarantine", srcDir)
	if err != nil {
		return hostfs.QuarantineMove{}, err
	}
	dest := filepath.Clean(destDir)
	if !filepath.IsAbs(dest) || isSubpath(dest, src) || isSubpath(src, dest) {
		return hostfs.QuarantineMove{}, fmt.Errorf("quarantine %s: invalid destination %s", src, dest)
	}
	if err := m.refuseMounted(ctx, src); err != nil {
		return hostfs.QuarantineMove{}, err
	}
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return hostfs.QuarantineMove{}, nil
		}
		return hostfs.QuarantineMove{}, fmt.Errorf("quarantine %s: lstat: %w", src, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return hostfs.QuarantineMove{}, fmt.Errorf("quarantine %s: not a directory", src)
	}
	ents, err := readDirents(src, 0)
	if err != nil {
		return hostfs.QuarantineMove{}, fmt.Errorf("quarantine %s: %w", src, sanitize("list", err))
	}
	if len(ents) == 0 {
		return hostfs.QuarantineMove{}, nil
	}
	if err := m.refuseMounted(ctx, src); err != nil {
		return hostfs.QuarantineMove{}, err
	}
	created, err := createExclusive(dest)
	if err != nil {
		return hostfs.QuarantineMove{}, fmt.Errorf("quarantine %s: %w", src, err)
	}
	mv := hostfs.QuarantineMove{Destination: created}
	rename := m.Rename
	if rename == nil {
		rename = os.Rename
	}
	var firstErr error
	failed := 0
	for i, e := range ents {
		if err := ctx.Err(); err != nil {
			return mv, fmt.Errorf("quarantine %s: moved %d of %d entries: %w", src, mv.Entries, len(ents), err)
		}
		from, to := filepath.Join(src, e.name), filepath.Join(created, e.name)
		err := rename(from, to)
		if errors.Is(err, unix.EXDEV) {
			err = copyThenRemove(from, to)
		} else {
			err = sanitize("rename", err)
		}
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("entry %d: %w", i+1, err)
			}
			continue
		}
		mv.Entries++
	}
	if firstErr != nil {
		return mv, fmt.Errorf("quarantine %s: %d of %d entries not moved, first: %w", src, failed, len(ents), firstErr)
	}
	return mv, nil
}

func (m *Mutator) refuseMounted(ctx context.Context, src string) error {
	snap, err := m.Mounts.Snapshot(ctx)
	if err != nil {
		return err
	}
	if mountedAtOrBelow(snap, src) {
		return fmt.Errorf("quarantine %s: %w", src, hostfs.ErrIsMountpoint)
	}
	return nil
}

// createExclusive creates dest (or the first free dest-<n>) with mkdir, which
// fails on an existing name, so two quarantines never share a folder.
func createExclusive(dest string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), quarantineDirMode); err != nil {
		return "", fmt.Errorf("create quarantine root: %w", err)
	}
	for n := 1; n <= maxCollisions; n++ {
		name := dest
		if n > 1 {
			name = dest + "-" + strconv.Itoa(n)
		}
		err := os.Mkdir(name, quarantineDirMode)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("create quarantine folder: %w", err)
		}
	}
	return "", fmt.Errorf("create quarantine folder %s: %d names taken", dest, maxCollisions)
}

// copyThenRemove moves an entry across filesystems: a complete copy first,
// and only then removal of the source. On any copy error the source is left
// untouched (the partial copy stays in the quarantine folder).
func copyThenRemove(from, to string) error {
	if err := copyTree(from, to); err != nil {
		return err
	}
	if err := os.RemoveAll(from); err != nil {
		return sanitize("remove source after copy", err)
	}
	return nil
}

func copyTree(from, to string) error {
	fi, err := os.Lstat(from)
	if err != nil {
		return sanitize("lstat", err)
	}
	switch mode := fi.Mode(); {
	case mode.IsRegular():
		if err := copyFile(from, to, mode.Perm()); err != nil {
			return err
		}
	case mode.IsDir():
		if err := os.Mkdir(to, mode.Perm()|0o700); err != nil {
			return sanitize("mkdir", err)
		}
		ents, err := readDirents(from, 0)
		if err != nil {
			return sanitize("list", err)
		}
		for _, e := range ents {
			if err := copyTree(filepath.Join(from, e.name), filepath.Join(to, e.name)); err != nil {
				return err
			}
		}
		if err := os.Chmod(to, mode.Perm()); err != nil {
			return sanitize("chmod", err)
		}
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(from)
		if err != nil {
			return sanitize("readlink", err)
		}
		if err := os.Symlink(target, to); err != nil {
			return sanitize("symlink", err)
		}
	default:
		return sanitize("copy", errUnsupportedType)
	}
	if st, ok := fi.Sys().(*unix.Stat_t); ok {
		_ = os.Lchown(to, int(st.Uid), int(st.Gid)) // best effort (root only)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		_ = os.Chtimes(to, fi.ModTime(), fi.ModTime())
	}
	return nil
}

func copyFile(from, to string, perm fs.FileMode) (err error) {
	in, err := os.Open(from) //nolint:gosec // path built from a listed entry
	if err != nil {
		return sanitize("open", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // see above
	if err != nil {
		return sanitize("create", err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = sanitize("close", cerr)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return sanitize("copy", err)
	}
	if err := out.Sync(); err != nil {
		return sanitize("fsync", err)
	}
	return nil
}
