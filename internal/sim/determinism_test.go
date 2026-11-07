package sim

import (
	"bytes"
	"testing"
)

// determinismRuns is how many times each scenario runs in TestDeterminism.
const determinismRuns = 10

// TestDeterminism runs every scenario determinismRuns times; every run must
// produce a byte-identical trace (about 6 minutes). Under -short it runs a
// representative subset, under -race only s04-wrong-fstype.
func TestDeterminism(t *testing.T) {
	subset := map[string]bool{
		"s04-wrong-fstype": true, "s09-cotenant-pinning": true, "s12a-worker-crash-mid-task": true,
		"s17-quarantine-lifecycle": true, "x01-two-node-scope-contention": true, "cp-crash-workload-releasing-mounts": true,
	}
	for _, s := range Scenarios() {
		if (testing.Short() && !subset[s.Name]) || (raceEnabled && s.Name != "s04-wrong-fstype") {
			continue
		}
		t.Run(s.Name, func(t *testing.T) {
			var first []byte
			for i := 0; i < determinismRuns; i++ {
				res, err := Run(s)
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					first = res.Trace
					continue
				}
				if !bytes.Equal(first, res.Trace) {
					t.Fatalf("run %d differs from run 1:\n%s", i+1, firstDiff(first, res.Trace))
				}
			}
		})
	}
}
