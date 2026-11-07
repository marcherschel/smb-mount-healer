package linux

import (
	"errors"
	"io"
	"syscall"
)

// entryError is an error about an entry inside a directory whose entry names
// are user data. It carries the operation and the errno only, never a path.
type entryError struct {
	op  string
	err error // a syscall.Errno or a path-free sentinel
}

func (e *entryError) Error() string { return e.op + ": " + e.err.Error() }
func (e *entryError) Unwrap() error { return e.err }

var errWithheld = errors.New("error details withheld (may contain user data names)")

// sanitize strips every path from err (fs.PathError, os.LinkError,
// os.SyscallError, ...) and keeps only the errno or a known path-free
// sentinel.
func sanitize(op string, err error) error {
	if err == nil {
		return nil
	}
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno):
		return &entryError{op: op, err: errno}
	case errors.Is(err, io.ErrShortWrite):
		return &entryError{op: op, err: io.ErrShortWrite}
	case errors.Is(err, io.ErrUnexpectedEOF):
		return &entryError{op: op, err: io.ErrUnexpectedEOF}
	case errors.Is(err, errUnsupportedType):
		return &entryError{op: op, err: errUnsupportedType}
	}
	return &entryError{op: op, err: errWithheld}
}
