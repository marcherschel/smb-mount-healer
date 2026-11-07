package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// DefaultExe is the binary the runner re-executes: the running healer itself.
const DefaultExe = "/proc/self/exe"

// DefaultKillGrace is how long the runner waits for a SIGKILLed child to be
// reaped before declaring it hung (stuck in D-state).
const DefaultKillGrace = 500 * time.Millisecond

// maxChildOutput bounds what the parent keeps of a child's stdout/stderr.
const maxChildOutput = 4096

// Options configure a Runner.
type Options struct {
	// Timeout bounds each child (STAT_TIMEOUT). Required.
	Timeout time.Duration
	// ExpectedFSTypes are the healthy fstypes (EXPECTED_FSTYPES). Required for Probe.
	ExpectedFSTypes []string
	// Exe is the binary to run; DefaultExe if empty.
	Exe string
	// ArgsPrefix is inserted before "--probe <op> <path>" (tests use it to
	// steer the test binary into its helper mode).
	ArgsPrefix []string
	// Env is the child environment; nil inherits the parent's.
	Env []string
	// KillGrace overrides DefaultKillGrace.
	KillGrace time.Duration
}

// Runner spawns probe children and implements hostfs.Prober. It keeps at most
// one live probe child (stat/statfs) and one live umount child per path. A
// child that does not exit after SIGKILL is "hung": it stays tracked until
// the kernel finally lets it die, and every Probe of that path returns
// ProbeHung without spawning a new child.
type Runner struct {
	opts Options
	// kill is a test hook; defaults to SIGKILL.
	kill func(*os.Process) error

	mu   sync.Mutex
	live map[childKey]*child
	wg   sync.WaitGroup
}

var _ hostfs.Prober = (*Runner)(nil)

type childKey struct {
	umount bool
	path   string
}

type child struct {
	pid     int
	started time.Time
	done    chan struct{}
	stdout  boundedBuffer
	stderr  boundedBuffer
	process *os.Process
	waitErr error // set before done is closed
	hung    bool  // guarded by Runner.mu
}

// NewRunner returns a Runner. It panics if opts.Timeout is not positive.
func NewRunner(opts Options) *Runner {
	if opts.Timeout <= 0 {
		panic("probe: Options.Timeout must be positive")
	}
	if opts.Exe == "" {
		opts.Exe = DefaultExe
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = DefaultKillGrace
	}
	return &Runner{
		opts: opts,
		kill: func(p *os.Process) error { return p.Kill() },
		live: map[childKey]*child{},
	}
}

// Hung returns the number of children that did not exit after SIGKILL and
// are still alive (metric probes_hung).
func (r *Runner) Hung() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.live {
		if c.hung {
			n++
		}
	}
	return n
}

// Live returns the number of children currently alive (hung or not).
func (r *Runner) Live() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// Close SIGKILLs every live child and waits until all of them are reaped or
// ctx is done. A child stuck in D-state can only be reaped by the kernel, so
// Close returns ctx.Err() in that case.
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	for _, c := range r.live {
		_ = r.kill(c.process)
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Probe implements hostfs.Prober: structural checks against snap first
// (not_mountpoint, wrong_fstype), then stat and statfs, each in a child
// bounded by Timeout.
func (r *Runner) Probe(ctx context.Context, path string, snap hostfs.MountSnapshot) hostfs.ProbeResult {
	start := time.Now()
	res := hostfs.ProbeResult{Path: path}
	finish := func(class hostfs.ProbeClass, detail string) hostfs.ProbeResult {
		res.Class, res.Detail, res.Duration = class, detail, time.Since(start)
		return res
	}

	e, ok := snap.Lookup(path)
	if !ok {
		return finish(hostfs.ProbeNotMountpoint, "not a mountpoint in mountinfo")
	}
	res.FSType = e.FSType
	if !slices.Contains(r.opts.ExpectedFSTypes, e.FSType) {
		return finish(hostfs.ProbeWrongFSType, fmt.Sprintf("fstype %q (source %q), expected one of %q",
			e.FSType, e.Source, strings.Join(r.opts.ExpectedFSTypes, " ")))
	}

	// stat alone is not enough: newer CIFS clients (seen on 6.17/6.18) answer
	// stat of the share root from cache while the server is gone. statfs is
	// never cached and always round-trips to the server.
	for _, op := range []string{OpStat, OpStatfs} {
		out := r.run(ctx, childKey{path: path}, op)
		switch out.kind {
		case outcomeDone:
			if !out.res.OK {
				return finish(hostfs.ProbeStatError, op+": "+out.res.Error)
			}
		case outcomeBusy, outcomeHung:
			return finish(hostfs.ProbeHung, out.detail)
		case outcomeTimeout:
			return finish(hostfs.ProbeStatTimeout, out.detail)
		case outcomeCanceled, outcomeFailed:
			return finish(hostfs.ProbeError, out.detail)
		default:
			return finish(hostfs.ProbeError, "unexpected probe outcome")
		}
	}
	return finish(hostfs.ProbeOK, "")
}

// Umount runs umount2(path, MNT_FORCE|MNT_DETACH) in a child. A failed
// syscall is returned wrapping its unix.Errno; a previous umount child of
// path that is still alive, or this one hanging, wraps hostfs.ErrHung.
func (r *Runner) Umount(ctx context.Context, path string) error {
	out := r.run(ctx, childKey{umount: true, path: path}, OpUmount)
	switch out.kind {
	case outcomeDone:
		if out.res.OK {
			return nil
		}
		return fmt.Errorf("umount: %w", unix.Errno(out.res.Errno)) //nolint:gosec // errno from the child is small and positive
	case outcomeBusy, outcomeHung:
		return fmt.Errorf("%w: %s", hostfs.ErrHung, out.detail)
	case outcomeTimeout, outcomeCanceled, outcomeFailed:
		return errors.New(out.detail)
	}
	return errors.New("unexpected umount outcome")
}

type outcomeKind int

const (
	outcomeDone     outcomeKind = iota // child exited with a parsed result
	outcomeBusy                        // a previous child for the key is alive; nothing spawned
	outcomeTimeout                     // killed after Timeout and reaped
	outcomeHung                        // killed but not reaped within KillGrace
	outcomeCanceled                    // ctx done; killed
	outcomeFailed                      // spawn failure, bad exit or unparsable output
)

type outcome struct {
	kind   outcomeKind
	res    ChildResult
	detail string
}

func (r *Runner) run(ctx context.Context, key childKey, op string) outcome {
	r.mu.Lock()
	if prev, ok := r.live[key]; ok {
		state := "still running"
		if prev.hung {
			state = "hung (did not exit after SIGKILL)"
		}
		r.mu.Unlock()
		return outcome{kind: outcomeBusy, detail: fmt.Sprintf("previous %s child pid %d %s for %s",
			op, prev.pid, state, time.Since(prev.started).Round(time.Millisecond))}
	}
	args := make([]string, 0, len(r.opts.ArgsPrefix)+3)
	args = append(args, r.opts.ArgsPrefix...)
	args = append(args, "--probe", op, key.path)
	cmd := exec.Command(r.opts.Exe, args...) //nolint:gosec // re-executes ourselves with a fixed op
	cmd.Env = r.opts.Env
	// Pdeathsig: the child dies with us. Setpgid keeps terminal signals away.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	c := &child{started: time.Now(), done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &c.stdout, &c.stderr
	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		return outcome{kind: outcomeFailed, detail: fmt.Sprintf("spawn %s child: %v", op, err)}
	}
	c.pid, c.process = cmd.Process.Pid, cmd.Process
	r.live[key] = c
	r.wg.Add(1)
	r.mu.Unlock()

	// Reaper: the only goroutine that waits for the child. It may outlive
	// this call by a long time if the child is stuck in D-state.
	go func() {
		defer r.wg.Done()
		c.waitErr = cmd.Wait()
		r.mu.Lock()
		if r.live[key] == c {
			delete(r.live, key)
		}
		r.mu.Unlock()
		close(c.done)
	}()

	timer := time.NewTimer(r.opts.Timeout)
	defer timer.Stop()
	var why outcomeKind
	select {
	case <-c.done:
		return parseOutcome(op, c)
	case <-timer.C:
		why = outcomeTimeout
	case <-ctx.Done():
		why = outcomeCanceled
	}

	_ = r.kill(c.process)
	grace := time.NewTimer(r.opts.KillGrace)
	defer grace.Stop()
	select {
	case <-c.done:
		if why == outcomeCanceled {
			return outcome{kind: outcomeCanceled, detail: fmt.Sprintf("%s canceled: %v", op, ctx.Err())}
		}
		return outcome{kind: outcomeTimeout, detail: fmt.Sprintf("%s exceeded %s, child killed", op, r.opts.Timeout)}
	case <-grace.C:
		r.mu.Lock()
		c.hung = true
		r.mu.Unlock()
		return outcome{kind: outcomeHung, detail: fmt.Sprintf("%s child pid %d did not exit %s after SIGKILL (timeout %s)",
			op, c.pid, r.opts.KillGrace, r.opts.Timeout)}
	}
}

func parseOutcome(op string, c *child) outcome {
	if c.waitErr != nil {
		return outcome{kind: outcomeFailed, detail: fmt.Sprintf("%s child: %v: %s", op, c.waitErr, firstLine(c.stderr.String()))}
	}
	var res ChildResult
	if err := json.Unmarshal(c.stdout.Bytes(), &res); err != nil || res.Op != op {
		return outcome{kind: outcomeFailed, detail: fmt.Sprintf("%s child: unparsable result", op)}
	}
	return outcome{kind: outcomeDone, res: res}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	const maxLen = 200
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// boundedBuffer keeps the first maxChildOutput bytes and discards the rest.
// It is written by exec's copy goroutine and read only after Wait returned.
type boundedBuffer struct {
	b []byte
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	if room := maxChildOutput - len(w.b); room > 0 {
		w.b = append(w.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (w *boundedBuffer) Bytes() []byte  { return w.b }
func (w *boundedBuffer) String() string { return string(w.b) }
