package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// TestMain doubles as the probe child: when the runner re-executes the test
// binary with "--probe <op> <path>", the path selects a helper behaviour.
//
//	/helper/hang            sleep forever (until SIGKILL)
//	/helper/sleep/<dur>/... sleep <dur>, then a real probe of "/"
//	/helper/garbage         print non-JSON and exit 0
//	/helper/exit/<n>        exit with code n
//	/helper/statfs-enoent   stat of "/" succeeds, statfs fails with ENOENT
//	anything else           the real RunChild
func TestMain(m *testing.M) {
	if i := slices.Index(os.Args, "--probe"); i > 0 && len(os.Args) == i+3 {
		os.Exit(helperChild(os.Args[i+1], os.Args[i+2]))
	}
	goleak.VerifyTestMain(m)
}

func helperChild(op, path string) int {
	switch {
	case path == "/helper/hang":
		select {}
	case strings.HasPrefix(path, "/helper/sleep/"):
		d, err := time.ParseDuration(strings.Split(strings.TrimPrefix(path, "/helper/sleep/"), "/")[0])
		if err != nil {
			return 99
		}
		time.Sleep(d)
		return RunChild(op, []string{"/"})
	case path == "/helper/garbage":
		_, _ = os.Stdout.WriteString("not json\n")
		return 0
	case path == "/helper/statfs-enoent":
		if op == OpStatfs {
			return RunChild(op, []string{"/helper/nonexistent"})
		}
		return RunChild(op, []string{"/"})
	case strings.HasPrefix(path, "/helper/exit/"):
		var n int
		_, _ = fmt.Sscanf(strings.TrimPrefix(path, "/helper/exit/"), "%d", &n)
		return n
	}
	return RunChild(op, []string{path})
}

func newTestRunner(t *testing.T, timeout time.Duration) *Runner {
	t.Helper()
	r := NewRunner(Options{
		Timeout:         timeout,
		ExpectedFSTypes: []string{"cifs", "smb3"},
		Exe:             os.Args[0],
		KillGrace:       200 * time.Millisecond,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.kill = func(p *os.Process) error { return p.Kill() }
		if err := r.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return r
}

func cifsSnap(paths ...string) hostfs.MountSnapshot {
	var s hostfs.MountSnapshot
	for i, p := range paths {
		s.Entries = append(s.Entries, hostfs.MountEntry{ID: 100 + i, MountPoint: p, FSType: "cifs", Source: "//srv/share"})
	}
	return s
}

func TestProbeStructuralChecksSpawnNothing(t *testing.T) {
	r := newTestRunner(t, time.Second)
	r.opts.Exe = "/nonexistent" // any spawn would fail with probe_error
	snap := hostfs.MountSnapshot{Entries: []hostfs.MountEntry{
		{ID: 1, MountPoint: "/a", FSType: "cifs"},
		{ID: 2, MountPoint: "/a", FSType: "xfs", Source: "/dev/vda6"}, // overmount: last wins
	}}
	got := r.Probe(context.Background(), "/a", snap)
	if got.Class != hostfs.ProbeWrongFSType || got.FSType != "xfs" || !strings.Contains(got.Detail, "/dev/vda6") {
		t.Fatalf("overmount: %+v", got)
	}
	if got := r.Probe(context.Background(), "/b", snap); got.Class != hostfs.ProbeNotMountpoint {
		t.Fatalf("missing: %+v", got)
	}
}

func TestProbeOKAndStatError(t *testing.T) {
	r := newTestRunner(t, 10*time.Second)
	dir := t.TempDir()
	if got := r.Probe(context.Background(), dir, cifsSnap(dir)); got.Class != hostfs.ProbeOK || got.FSType != "cifs" {
		t.Fatalf("ok: %+v", got)
	}
	missing := dir + "/missing"
	got := r.Probe(context.Background(), missing, cifsSnap(missing))
	if got.Class != hostfs.ProbeStatError || !strings.Contains(got.Detail, "no such file") {
		t.Fatalf("stat error: %+v", got)
	}
	if r.Live() != 0 || r.Hung() != 0 {
		t.Fatalf("live=%d hung=%d", r.Live(), r.Hung())
	}
}

// TestProbeStatfsFailureIsStatError: stat answered from the client cache
// must not hide a server that no longer answers statfs.
func TestProbeStatfsFailureIsStatError(t *testing.T) {
	r := newTestRunner(t, 10*time.Second)
	p := "/helper/statfs-enoent"
	got := r.Probe(context.Background(), p, cifsSnap(p))
	if got.Class != hostfs.ProbeStatError || !strings.HasPrefix(got.Detail, "statfs: ") {
		t.Fatalf("statfs error: %+v", got)
	}
}

func TestProbeBadChild(t *testing.T) {
	r := newTestRunner(t, 10*time.Second)
	for _, p := range []string{"/helper/garbage", "/helper/exit/3"} {
		if got := r.Probe(context.Background(), p, cifsSnap(p)); got.Class != hostfs.ProbeError {
			t.Fatalf("%s: %+v", p, got)
		}
	}
}

func TestProbeTimeoutKillsChild(t *testing.T) {
	r := newTestRunner(t, 300*time.Millisecond)
	p := "/helper/hang"
	start := time.Now()
	got := r.Probe(context.Background(), p, cifsSnap(p))
	if got.Class != hostfs.ProbeStatTimeout {
		t.Fatalf("got %+v", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("probe blocked %s", d)
	}
	if r.Live() != 0 || r.Hung() != 0 {
		t.Fatalf("killed child still tracked: live=%d hung=%d", r.Live(), r.Hung())
	}
}

func TestProbeContextCanceled(t *testing.T) {
	r := newTestRunner(t, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p := "/helper/hang"
	if got := r.Probe(ctx, p, cifsSnap(p)); got.Class != hostfs.ProbeError {
		t.Fatalf("got %+v", got)
	}
}

// A child that survives SIGKILL (D-state) is hung: it is counted by Hung(),
// and further probes of the path return ProbeHung without spawning.
func TestProbeHungChildBlocksPath(t *testing.T) {
	r := newTestRunner(t, 200*time.Millisecond)
	r.kill = func(*os.Process) error { return nil } // simulate an unkillable child
	p := "/helper/hang"
	snap := cifsSnap(p, "/helper/sleep/0s/other")

	if got := r.Probe(context.Background(), p, snap); got.Class != hostfs.ProbeHung {
		t.Fatalf("first: %+v", got)
	}
	if r.Hung() != 1 || r.Live() != 1 {
		t.Fatalf("hung=%d live=%d", r.Hung(), r.Live())
	}
	start := time.Now()
	got := r.Probe(context.Background(), p, snap)
	if got.Class != hostfs.ProbeHung || !strings.Contains(got.Detail, "did not exit after SIGKILL") {
		t.Fatalf("second: %+v", got)
	}
	if time.Since(start) > 100*time.Millisecond || r.Live() != 1 {
		t.Fatalf("second probe spawned or blocked: live=%d", r.Live())
	}
	// Other paths are unaffected.
	r.opts.Timeout = 10 * time.Second // child start-up under -race can exceed 200ms
	if got := r.Probe(context.Background(), "/helper/sleep/0s/other", snap); got.Class != hostfs.ProbeOK {
		t.Fatalf("other path: %+v", got)
	}
	// Once the kernel lets the child die, the path is probed again.
	r.mu.Lock()
	r.kill = func(p *os.Process) error { return p.Kill() }
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if r.Hung() != 0 || r.Live() != 0 {
		t.Fatalf("after reap: hung=%d live=%d", r.Hung(), r.Live())
	}
}

func TestProbeSingleChildPerPath(t *testing.T) {
	r := newTestRunner(t, 10*time.Second)
	p := "/helper/sleep/700ms/x"
	snap := cifsSnap(p)
	var wg sync.WaitGroup
	results := make(chan hostfs.ProbeResult, 1)
	wg.Go(func() { results <- r.Probe(context.Background(), p, snap) })
	deadline := time.Now().Add(5 * time.Second)
	for r.Live() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := r.Probe(context.Background(), p, snap)
	if got.Class != hostfs.ProbeHung || !strings.Contains(got.Detail, "still running") {
		t.Fatalf("concurrent probe: %+v", got)
	}
	if r.Hung() != 0 {
		t.Fatalf("a running (not killed) child must not count as hung: %d", r.Hung())
	}
	wg.Wait()
	if first := <-results; first.Class != hostfs.ProbeOK {
		t.Fatalf("first probe: %+v", first)
	}
}

func TestUmountErrors(t *testing.T) {
	r := newTestRunner(t, 10*time.Second)
	err := r.Umount(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("umount of a plain dir succeeded")
	}
	var errno unix.Errno
	if !errors.As(err, &errno) || (errno != unix.EINVAL && errno != unix.EPERM) {
		t.Fatalf("err = %v", err)
	}

	r.kill = func(*os.Process) error { return nil }
	r.opts.Timeout = 200 * time.Millisecond
	if err := r.Umount(context.Background(), "/helper/hang"); !errors.Is(err, hostfs.ErrHung) {
		t.Fatalf("hung umount: %v", err)
	}
	if err := r.Umount(context.Background(), "/helper/hang"); !errors.Is(err, hostfs.ErrHung) {
		t.Fatalf("second umount: %v", err)
	}
	// A hung umount child does not block probing the same path.
	if got := r.Probe(context.Background(), "/helper/hang", cifsSnap("/helper/hang")); got.Class != hostfs.ProbeHung || r.Live() != 2 {
		t.Fatalf("probe: %+v live=%d", got, r.Live())
	}
}

func TestRunChild(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		op       string
		args     []string
		wantCode int
		wantOK   bool
		check    func(ChildResult) bool
	}{
		{op: OpStat, args: []string{dir}, wantCode: ExitResult, wantOK: true},
		{op: OpStatfs, args: []string{dir}, wantCode: ExitResult, wantOK: true, check: func(r ChildResult) bool { return r.FSMagic != 0 }},
		{op: OpStat, args: []string{dir + "/nope"}, wantCode: ExitResult, check: func(r ChildResult) bool { return r.Errno == int(unix.ENOENT) }},
		{op: OpUmount, args: []string{dir}, wantCode: ExitResult, check: func(r ChildResult) bool { return r.Errno != 0 }},
		{op: "rm", args: []string{dir}, wantCode: ExitUsage},
		{op: OpStat, args: []string{"relative"}, wantCode: ExitUsage},
		{op: OpStat, args: []string{dir, dir}, wantCode: ExitUsage},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		code := runChild(&out, &errOut, tt.op, tt.args)
		if code != tt.wantCode {
			t.Fatalf("%s %v: code %d, stderr %q", tt.op, tt.args, code, errOut.String())
		}
		if code != ExitResult {
			if out.Len() != 0 {
				t.Fatalf("%s: stdout on usage error", tt.op)
			}
			continue
		}
		var res ChildResult
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.OK != tt.wantOK || res.Op != tt.op || (tt.check != nil && !tt.check(res)) {
			t.Fatalf("%s %v: %+v", tt.op, tt.args, res)
		}
		if strings.Contains(out.String(), dir) {
			t.Fatalf("child output contains the path: %s", out.String())
		}
	}
}
