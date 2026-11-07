package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Child operations accepted after the hidden --probe flag.
const (
	OpStat   = "stat"
	OpStatfs = "statfs"
	OpUmount = "umount"
)

// Child exit codes. A syscall failure is NOT an exit failure: the child
// reports it in its JSON result and exits ExitResult.
const (
	// ExitResult means a ChildResult was written to stdout.
	ExitResult = 0
	// ExitUsage means the op or its arguments were invalid; nothing was run.
	ExitUsage = 2
	// ExitOutput means the result could not be written to stdout.
	ExitOutput = 3
)

// ChildResult is the single JSON line a child writes to stdout. It never
// contains a path or a directory listing: the parent knows the path.
type ChildResult struct {
	Op string `json:"op"`
	OK bool   `json:"ok"`
	// Errno is the raw errno of a failed syscall (0 on success).
	Errno int `json:"errno,omitempty"`
	// Error is strerror(Errno).
	Error string `json:"error,omitempty"`
	// FSMagic is statfs f_type (statfs only).
	FSMagic int64 `json:"fsMagic,omitempty"`
}

// umountFlags are the flags of the only unmount the healer ever does
// (umount -f -l in the bash healer).
const umountFlags = unix.MNT_FORCE | unix.MNT_DETACH

// RunChild is the entry point of the hidden "--probe <op> <path>" mode. It
// runs exactly one syscall, writes one ChildResult as JSON to stdout and
// returns the process exit code. It initialises nothing else (no logging, no
// clients), so a hang can only be in the syscall itself.
func RunChild(op string, args []string) int {
	return runChild(os.Stdout, os.Stderr, op, args)
}

func runChild(stdout, stderr io.Writer, op string, args []string) int {
	if len(args) != 1 || args[0] == "" || args[0][0] != '/' {
		_, _ = fmt.Fprintf(stderr, "probe %s: want exactly one absolute path argument\n", op)
		return ExitUsage
	}
	path := args[0]
	res := ChildResult{Op: op}
	var err error
	switch op {
	case OpStat:
		var st unix.Stat_t
		err = unix.Stat(path, &st)
	case OpStatfs:
		var st unix.Statfs_t
		err = unix.Statfs(path, &st)
		if err == nil {
			res.FSMagic = int64(st.Type) //nolint:unconvert // Type is int32 on some arches
		}
	case OpUmount:
		err = unix.Unmount(path, umountFlags)
	default:
		_, _ = fmt.Fprintf(stderr, "probe: unknown op %q\n", op)
		return ExitUsage
	}
	if err != nil {
		res.Error = err.Error()
		var errno unix.Errno
		if errors.As(err, &errno) {
			res.Errno = int(errno)
		}
	} else {
		res.OK = true
	}
	if err := json.NewEncoder(stdout).Encode(res); err != nil {
		return ExitOutput
	}
	return ExitResult
}
