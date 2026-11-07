package linux

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// dirent is one raw directory entry: the name and d_type from getdents64,
// without any stat of the entry. A name inside a quarantined directory must
// never leave this package.
type dirent struct {
	name string
	typ  uint8 // unix.DT_*
}

// readDirents lists dir with getdents64 only (no lstat of entries, so a
// mountpoint among the entries is never touched). Symlinks are not followed
// (O_NOFOLLOW). max > 0 stops after max entries. Errors are returned as
// errors wrapping the errno and never contain a name from inside dir.
func readDirents(dir string, maxEntries int) ([]dirent, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open directory: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	var out []dirent
	buf := make([]byte, 32*1024)
	for {
		n, err := unix.Getdents(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("getdents: %w", err)
		}
		if n <= 0 {
			return out, nil
		}
		b := buf[:n]
		for len(b) > 0 {
			// struct linux_dirent64 { u64 ino; s64 off; u16 reclen; u8 type; char name[]; }
			if len(b) < 19 {
				return nil, errors.New("getdents: short record")
			}
			reclen := int(binary.NativeEndian.Uint16(b[16:18]))
			if reclen < 19 || reclen > len(b) {
				return nil, errors.New("getdents: bad record length")
			}
			typ := b[18]
			name := b[19:reclen]
			for i, c := range name {
				if c == 0 {
					name = name[:i]
					break
				}
			}
			if s := string(name); s != "." && s != ".." {
				out = append(out, dirent{name: s, typ: typ})
				if maxEntries > 0 && len(out) >= maxEntries {
					return out, nil
				}
			}
			b = b[reclen:]
		}
	}
}
