package health

import (
	"time"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// State is the health state (same values as SmbMount.status.health).
type State = v1alpha1.Health

// Reason is the failure reason (same values as SmbMount.status.reason).
type Reason = v1alpha1.FailureReason

// Record is the full FSM state of one mount.
type Record struct {
	State        State
	Reason       Reason    // set for Suspect, Broken, Errored
	FirstFailure time.Time // zero unless failing
	Failures     int32     // consecutive failures
	// LastClass is the class of the streak's latest failure (a probe_error
	// is no observation and keeps it); probe_error when there is no streak.
	LastClass hostfs.ProbeClass
	// RecoveringSince is the first ok probe after the last failure of a slow
	// streak that is not Broken; zero otherwise. See Step.
	RecoveringSince time.Time
}

// Recovering reports whether r is a slow streak whose last probe was ok,
// waiting out SlowRecoveryWindow.
func (r Record) Recovering() bool { return !r.RecoveringSince.IsZero() }

// Config is the debounce configuration.
type Config struct {
	RecheckAttempts   int32         // RECHECK_ATTEMPTS
	SlowConfirmWindow time.Duration // SLOW_CONFIRM_WINDOW
	// SlowRecoveryWindow (SLOW_RECOVERY_WINDOW): how long a mount with a slow
	// failure streak must probe ok before the streak ends. 0: the first ok
	// probe ends it.
	SlowRecoveryWindow time.Duration
}

// Transition is emitted when Step changes State.
type Transition struct {
	From, To State
	Reason   Reason
	// FalseAlarm is true for Suspect -> Healthy.
	FalseAlarm bool
}
