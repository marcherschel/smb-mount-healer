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
	LastClass    hostfs.ProbeClass
}

// Config is the debounce configuration.
type Config struct {
	RecheckAttempts   int32         // RECHECK_ATTEMPTS
	SlowConfirmWindow time.Duration // SLOW_CONFIRM_WINDOW
}

// Transition is emitted when Step changes State.
type Transition struct {
	From, To State
	Reason   Reason
	// FalseAlarm is true for Suspect -> Healthy.
	FalseAlarm bool
}
