package sim

import (
	"strings"
	"testing"
	"time"
)

// Repros of issues the simulator found in other packages. Each is skipped
// with the reason until the package is fixed; remove the Skip to verify a
// fix (and regenerate the affected golden traces with make sim-update).

func scenarioByName(t *testing.T, name string) Scenario {
	t.Helper()
	for _, s := range Scenarios() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no scenario %s", name)
	return Scenario{}
}

// TestBugSpuriousSkippedNoOwner: two nodes trigger for the same Deployment
// in one round (x01). n2's remediation is created before n1's has a scope,
// waits in pending with ScopeConflict, and when n1 scales the Deployment
// down its broken pod is terminating, so pending -> planning reports
// "no_scope_conflict", planning finds no owner and the episode ends
// skipped_no_owner with a Warning "manual restart needed". That terminal
// remediation then backs n2 off for BACKOFF_BASE, so n2 heals 5 minutes
// late. Expected: n2's remediation either keeps waiting for n1 or ends
// without counting towards backoff, and the trigger records a
// scope_conflict suppression.
func TestBugSpuriousSkippedNoOwner(t *testing.T) {
	s := scenarioByName(t, "x01-two-node-scope-contention")
	s.Expect = []string{"Workload:healed", "Workload:healed"}
	s.Check = func(w *World) []string {
		// n2's remediation must wait for n1 instead of ending
		// skipped_no_owner, so nothing on n2 is held back by backoff.
		var p []string
		if strings.Contains(string(w.trace.Bytes()), "skipped_no_owner") {
			p = append(p, "a remediation ended skipped_no_owner")
		}
		if strings.Contains(string(w.trace.Bytes()), "remediation_suppressed_total{reason=backoff}") {
			p = append(p, "a finding was suppressed by backoff")
		}
		return p
	}
	runRepro(t, s)
}

// TestBugNoOpSweepBacksOff: a StatefulSet pod is recreated under the same
// name (s05). For one publish there is no pod on the node, the detached
// globalmount dir holds data, and a Sweep is created; its planning probe
// sees the new pod and ends nothing_to_sweep ("healed", nothing done). That
// no-op episode still counts as a remediation of the PV, so the Workload
// remediation for the new pod's wrong_fstype mount is suppressed by backoff
// for BACKOFF_BASE (created at 00:07:15 instead of ~00:03:21).
func TestBugNoOpSweepBacksOff(t *testing.T) {
	s := scenarioByName(t, "s05-statefulset")
	s.Check = func(w *World) []string {
		// The Workload remediation must start right after the mount is
		// Broken (00:03:21), not after the backoff.
		if !strings.Contains(string(w.trace.Bytes()), `{"t":"00:03:21","kind":"smbr"`) {
			return []string{"Workload remediation not created when the mount broke (backoff from the no-op Sweep)"}
		}
		return nil
	}
	runRepro(t, s)
}

// TestBugSweepWaitsForUntrackedGlobalmountDir: a dead orphan globalmount
// that kubelet never staged (s08, mounted by hand) is force-unmounted by the
// Sweep, leaving an empty directory that nobody removes. The Sweep then waits
// the full QUARANTINE_UNSTAGE_WAIT (5m) in awaiting_unstage_completion and
// emits Warning UnstagePinned, although an empty unmounted globalmount dir
// does not block NodeStage. Expected: an empty, unmounted globalmount dir
// counts as unstaged.
func TestBugSweepWaitsForUntrackedGlobalmountDir(t *testing.T) {
	s := scenarioByName(t, "s08-dead-orphan-globalmount")
	s.Check = func(w *World) []string {
		var p []string
		if strings.Contains(string(w.trace.Bytes()), "UnstagePinned") {
			p = append(p, "UnstagePinned emitted for an empty globalmount dir")
		}
		if strings.Contains(string(w.trace.Bytes()), "unstage_timeout") {
			p = append(p, "sweep waited QUARANTINE_UNSTAGE_WAIT")
		}
		return p
	}
	runRepro(t, s)
}

// TestBugQuarantineProvenanceLostOnWorkerCrash: the worker crashes after a
// Quarantine action moved the data but before its result was persisted
// (s12a). The re-run finds the source empty (Succeeded without destination)
// and the in-memory provenance is gone, so the new SmbQuarantine has no
// sourcePath and no remediationRef: it looks like an adopted bash folder.
func TestBugQuarantineProvenanceLostOnWorkerCrash(t *testing.T) {
	s := scenarioByName(t, "s12a-worker-crash-mid-task")
	s.Check = func(w *World) []string {
		for _, q := range w.Quarantines() {
			if q.Spec.RemediationRef == "" || q.Spec.SourcePath == "" {
				return []string{"SmbQuarantine " + q.Name + " lost its provenance"}
			}
		}
		return nil
	}
	runRepro(t, s)
}

func runRepro(t *testing.T, s Scenario) {
	t.Helper()
	if s.Max == 0 {
		s.Max = 30 * time.Minute
	}
	res, err := Run(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Problems {
		t.Error(p)
	}
}
