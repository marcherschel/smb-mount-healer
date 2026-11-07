// Package health is the PURE per-mount debounce state machine.
//
// States: Unknown, Healthy, Suspect{firstFailure, n}, Broken{reason}, Errored.
//   - Broken when n >= RecheckAttempts AND either the class is fast/structural
//     (not_mountpoint, wrong_fstype, stat_error) or slow failures
//     (stat_timeout, probe_hung) have lasted SlowConfirmWindow.
//   - Errored on probe_error; never triggers remediation.
//   - A success while Suspect is a false alarm (metric false_alarms_total).
//
// State is rehydrated from SmbMount.status (health, reason, debounce) via
// FromStatus, so a worker restart neither resets nor re-triggers anything.
//
// Purity (lint-enforced by forbidigo/depguard): no os, no time.Now/Sleep,
// no math/rand, no controller-runtime or client-go. Time is an input.
package health
