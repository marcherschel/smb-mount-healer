package health

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// classKind groups probe classes by how Step treats them.
type classKind int

const (
	kindOK classKind = iota
	// kindFast: structural (not_mountpoint, wrong_fstype) and fast
	// (stat_error) failures; confirmed after RecheckAttempts in a row.
	kindFast
	// kindSlow: stat_timeout, probe_hung; additionally need SlowConfirmWindow.
	kindSlow
	// kindProbeError: the probe machinery failed; says nothing about the mount.
	kindProbeError
)

// classify maps a probe class to its kind. Unrecognised classes (including
// the empty string) are treated as probe_error: they can never cause Broken.
func classify(c hostfs.ProbeClass) classKind {
	switch c {
	case hostfs.ProbeOK:
		return kindOK
	case hostfs.ProbeNotMountpoint, hostfs.ProbeWrongFSType, hostfs.ProbeStatError:
		return kindFast
	case hostfs.ProbeStatTimeout, hostfs.ProbeHung:
		return kindSlow
	case hostfs.ProbeError:
		return kindProbeError
	default:
		return kindProbeError
	}
}

// Step advances the FSM with one probe result observed at now. It returns
// the next record and a non-nil Transition iff the state changed.
//
// Rules:
//   - ok: -> Healthy, debounce cleared. Suspect -> Healthy is a FalseAlarm;
//     Broken -> Healthy is a recovery. Transition.Reason is the previous reason.
//   - failure (not_mountpoint, wrong_fstype, stat_error, stat_timeout,
//     probe_hung): consecutive failures n = prev.Failures+1 (FirstFailure is
//     kept; it is set to now when the streak starts). Broken when
//     n >= RecheckAttempts and the class is fast/structural, or the class is
//     slow and now-FirstFailure >= SlowConfirmWindow; otherwise Suspect.
//     Once Broken, further failures of any class keep it Broken (Reason
//     follows the latest class).
//   - probe_error (or an unknown class): -> Errored with Reason probe_error.
//     It is "no observation" for debounce purposes: Failures and
//     FirstFailure are preserved, so a following failure continues the
//     streak, and Errored never becomes Broken by itself.
//
// A zero Record (empty State) is treated as Unknown. RecheckAttempts < 1 is
// treated as 1.
func Step(prev Record, class hostfs.ProbeClass, now time.Time, cfg Config) (Record, *Transition) {
	if prev.State == "" {
		prev.State = v1alpha1.HealthUnknown
	}
	var next Record
	switch classify(class) {
	case kindOK:
		next = Record{State: v1alpha1.HealthHealthy}
	case kindProbeError:
		next = Record{
			State:        v1alpha1.HealthErrored,
			Reason:       v1alpha1.ReasonProbeError,
			FirstFailure: prev.FirstFailure,
			Failures:     prev.Failures,
			LastClass:    hostfs.ProbeError,
		}
		if next.Failures <= 0 {
			next.Failures, next.FirstFailure = 0, time.Time{}
		}
	case kindFast, kindSlow:
		next = stepFailure(prev, class, now, cfg)
	}
	if next.State == prev.State {
		return next, nil
	}
	tr := &Transition{From: prev.State, To: next.State, Reason: next.Reason}
	if next.State == v1alpha1.HealthHealthy {
		tr.Reason = prev.Reason
		tr.FalseAlarm = prev.State == v1alpha1.HealthSuspect
	}
	return next, tr
}

func stepFailure(prev Record, class hostfs.ProbeClass, now time.Time, cfg Config) Record {
	next := Record{
		Reason:       Reason(class),
		LastClass:    class,
		Failures:     1,
		FirstFailure: now,
	}
	if prev.Failures > 0 {
		next.Failures = prev.Failures + 1
		if !prev.FirstFailure.IsZero() {
			next.FirstFailure = prev.FirstFailure
		}
	}
	attempts := max(cfg.RecheckAttempts, 1)
	confirmed := next.Failures >= attempts &&
		(classify(class) == kindFast || now.Sub(next.FirstFailure) >= cfg.SlowConfirmWindow)
	if confirmed || prev.State == v1alpha1.HealthBroken {
		next.State = v1alpha1.HealthBroken
	} else {
		next.State = v1alpha1.HealthSuspect
	}
	return next
}

// FromStatus rehydrates a Record from SmbMount.status. An empty or
// unrecognised health is Unknown. Healthy and Unknown carry no debounce.
func FromStatus(h v1alpha1.Health, r v1alpha1.FailureReason, d *v1alpha1.Debounce) Record {
	switch h {
	case v1alpha1.HealthSuspect, v1alpha1.HealthBroken, v1alpha1.HealthErrored:
	case v1alpha1.HealthHealthy:
		return Record{State: v1alpha1.HealthHealthy}
	case v1alpha1.HealthUnknown:
		return Record{State: v1alpha1.HealthUnknown}
	default:
		return Record{State: v1alpha1.HealthUnknown}
	}
	rec := Record{State: h, Reason: r}
	if d != nil {
		rec.Failures = max(d.ConsecutiveFailures, 0)
		rec.LastClass = hostfs.ProbeClass(d.LastClass)
		if d.FirstFailureTime != nil {
			rec.FirstFailure = d.FirstFailureTime.Time
		}
	}
	return rec
}

// ToStatus converts a Record into the SmbMount.status fields it persists.
// The debounce is nil when there is nothing to persist (Healthy, Unknown).
// An empty State is reported as Unknown.
func (r Record) ToStatus() (v1alpha1.Health, v1alpha1.FailureReason, *v1alpha1.Debounce) {
	h := r.State
	if h == "" {
		h = v1alpha1.HealthUnknown
	}
	if r.Failures == 0 && r.FirstFailure.IsZero() && r.LastClass == "" {
		return h, r.Reason, nil
	}
	d := &v1alpha1.Debounce{
		ConsecutiveFailures: r.Failures,
		LastClass:           v1alpha1.FailureReason(r.LastClass),
	}
	if !r.FirstFailure.IsZero() {
		t := metav1.NewTime(r.FirstFailure)
		d.FirstFailureTime = &t
	}
	return h, r.Reason, d
}
