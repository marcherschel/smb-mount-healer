package sim

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update rewrites the golden traces (make sim-update: go test ./internal/sim/... -args -update).
var update = flag.Bool("update", false, "rewrite internal/sim/testdata/traces/*.golden")

func goldenPath(name string) string { return filepath.Join("testdata", "traces", name+".golden") }

// TestScenarios runs every scenario once, checks its designed outcome and
// invariants, and compares its JSONL trace with the golden file.
//
// Under -race (make test) only raceSubset runs: the simulator is single
// threaded and ~20x slower with the race detector. Run `go test
// ./internal/sim/...` without -race for the whole suite.
func TestScenarios(t *testing.T) {
	for _, s := range Scenarios() {
		t.Run(s.Name, func(t *testing.T) {
			if raceEnabled && !raceSubset[s.Name] {
				t.Skip("race detector: only the race subset runs; run without -race for the full suite")
			}
			res, err := Run(s)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range res.Problems {
				t.Error(p)
			}
			path := goldenPath(s.Name)
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, res.Trace, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run make sim-update): %v", err)
			}
			if !bytes.Equal(want, res.Trace) {
				t.Errorf("trace differs from %s (run make sim-update and review the diff):\n%s", path, firstDiff(want, res.Trace))
			}
		})
	}
}

// raceSubset are the scenarios run under -race: the worker's concurrent
// probes, a worker crash and two nodes.
var raceSubset = map[string]bool{"s04-wrong-fstype": true, "s12a-worker-crash-mid-task": true, "x01-two-node-scope-contention": true}

// firstDiff shows the first differing line.
func firstDiff(want, got []byte) string {
	wl := strings.Split(string(want), "\n")
	gl := strings.Split(string(got), "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var a, b string
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			b = gl[i]
		}
		if a != b {
			return "line " + itoa(i+1) + ":\n  want: " + a + "\n  got:  " + b
		}
	}
	return "(identical lines, different bytes)"
}

func itoa(i int) string {
	var b [20]byte
	n := len(b)
	for {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			break
		}
	}
	return string(b[n:])
}
