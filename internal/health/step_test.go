package health

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

var (
	t0  = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cfg = Config{RecheckAttempts: 3, SlowConfirmWindow: 3 * time.Minute}
)

const (
	healthy = v1alpha1.HealthHealthy
	suspect = v1alpha1.HealthSuspect
	broken  = v1alpha1.HealthBroken
	errored = v1alpha1.HealthErrored
	unknown = v1alpha1.HealthUnknown
)

func failing(s State, c hostfs.ProbeClass, n int32, first time.Time) Record {
	return Record{State: s, Reason: Reason(c), FirstFailure: first, Failures: n, LastClass: c}
}

func TestStep(t *testing.T) {
	tests := []struct {
		name  string
		prev  Record
		class hostfs.ProbeClass
		now   time.Time
		cfg   *Config // nil: default cfg
		want  Record
		tr    *Transition
	}{
		// --- from Unknown / zero record ---
		{
			name: "zero record ok -> healthy", prev: Record{}, class: hostfs.ProbeOK, now: t0,
			want: Record{State: healthy}, tr: &Transition{From: unknown, To: healthy},
		},
		{
			name: "unknown ok -> healthy", prev: Record{State: unknown}, class: hostfs.ProbeOK, now: t0,
			want: Record{State: healthy}, tr: &Transition{From: unknown, To: healthy},
		},
		{
			name: "unknown fast failure -> suspect", prev: Record{State: unknown}, class: hostfs.ProbeNotMountpoint, now: t0,
			want: failing(suspect, hostfs.ProbeNotMountpoint, 1, t0),
			tr:   &Transition{From: unknown, To: suspect, Reason: v1alpha1.ReasonNotMountpoint},
		},
		{
			name: "unknown probe_error -> errored", prev: Record{State: unknown}, class: hostfs.ProbeError, now: t0,
			want: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
			tr:   &Transition{From: unknown, To: errored, Reason: v1alpha1.ReasonProbeError},
		},
		// --- from Healthy ---
		{
			name: "healthy ok stays healthy", prev: Record{State: healthy}, class: hostfs.ProbeOK, now: t0,
			want: Record{State: healthy},
		},
		{
			name: "healthy wrong_fstype -> suspect", prev: Record{State: healthy}, class: hostfs.ProbeWrongFSType, now: t0,
			want: failing(suspect, hostfs.ProbeWrongFSType, 1, t0),
			tr:   &Transition{From: healthy, To: suspect, Reason: v1alpha1.ReasonWrongFSType},
		},
		{
			name: "healthy stat_timeout -> suspect", prev: Record{State: healthy}, class: hostfs.ProbeStatTimeout, now: t0,
			want: failing(suspect, hostfs.ProbeStatTimeout, 1, t0),
			tr:   &Transition{From: healthy, To: suspect, Reason: v1alpha1.ReasonStatTimeout},
		},
		{
			name: "healthy fast failure with attempts=1 -> broken immediately", prev: Record{State: healthy},
			class: hostfs.ProbeStatError, now: t0, cfg: &Config{RecheckAttempts: 1, SlowConfirmWindow: time.Minute},
			want: failing(broken, hostfs.ProbeStatError, 1, t0),
			tr:   &Transition{From: healthy, To: broken, Reason: v1alpha1.ReasonStatError},
		},
		{
			name: "attempts=0 treated as 1", prev: Record{State: healthy},
			class: hostfs.ProbeNotMountpoint, now: t0, cfg: &Config{},
			want: failing(broken, hostfs.ProbeNotMountpoint, 1, t0),
			tr:   &Transition{From: healthy, To: broken, Reason: v1alpha1.ReasonNotMountpoint},
		},
		{
			name: "healthy probe_error -> errored", prev: Record{State: healthy}, class: hostfs.ProbeError, now: t0,
			want: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
			tr:   &Transition{From: healthy, To: errored, Reason: v1alpha1.ReasonProbeError},
		},
		{
			name: "unrecognised class treated as probe_error", prev: Record{State: healthy}, class: "bogus", now: t0,
			want: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
			tr:   &Transition{From: healthy, To: errored, Reason: v1alpha1.ReasonProbeError},
		},
		// --- Suspect, fast/structural classes ---
		{
			name: "suspect fast n=2 stays suspect", prev: failing(suspect, hostfs.ProbeNotMountpoint, 1, t0),
			class: hostfs.ProbeNotMountpoint, now: t0.Add(10 * time.Second),
			want: failing(suspect, hostfs.ProbeNotMountpoint, 2, t0),
		},
		{
			name: "suspect fast n=attempts -> broken", prev: failing(suspect, hostfs.ProbeWrongFSType, 2, t0),
			class: hostfs.ProbeWrongFSType, now: t0.Add(20 * time.Second),
			want: failing(broken, hostfs.ProbeWrongFSType, 3, t0),
			tr:   &Transition{From: suspect, To: broken, Reason: v1alpha1.ReasonWrongFSType},
		},
		{
			name: "suspect stat_error n=attempts -> broken without window", prev: failing(suspect, hostfs.ProbeStatError, 2, t0),
			class: hostfs.ProbeStatError, now: t0.Add(time.Second),
			want: failing(broken, hostfs.ProbeStatError, 3, t0),
			tr:   &Transition{From: suspect, To: broken, Reason: v1alpha1.ReasonStatError},
		},
		{
			name: "slow streak ending in fast class confirms on attempts", prev: failing(suspect, hostfs.ProbeStatTimeout, 2, t0),
			class: hostfs.ProbeStatError, now: t0.Add(20 * time.Second),
			want: failing(broken, hostfs.ProbeStatError, 3, t0),
			tr:   &Transition{From: suspect, To: broken, Reason: v1alpha1.ReasonStatError},
		},
		// --- Suspect, slow classes (window) ---
		{
			name: "slow n>=attempts inside window stays suspect", prev: failing(suspect, hostfs.ProbeStatTimeout, 2, t0),
			class: hostfs.ProbeStatTimeout, now: t0.Add(30 * time.Second),
			want: failing(suspect, hostfs.ProbeStatTimeout, 3, t0),
		},
		{
			name: "slow many attempts just before window stays suspect", prev: failing(suspect, hostfs.ProbeHung, 9, t0),
			class: hostfs.ProbeHung, now: t0.Add(3*time.Minute - time.Nanosecond),
			want: failing(suspect, hostfs.ProbeHung, 10, t0),
		},
		{
			name: "slow at window -> broken", prev: failing(suspect, hostfs.ProbeHung, 5, t0),
			class: hostfs.ProbeHung, now: t0.Add(3 * time.Minute),
			want: failing(broken, hostfs.ProbeHung, 6, t0),
			tr:   &Transition{From: suspect, To: broken, Reason: v1alpha1.ReasonProbeHung},
		},
		{
			name: "slow past window but n<attempts stays suspect", prev: failing(suspect, hostfs.ProbeStatTimeout, 1, t0),
			class: hostfs.ProbeStatTimeout, now: t0.Add(10 * time.Minute),
			want: failing(suspect, hostfs.ProbeStatTimeout, 2, t0),
		},
		{
			name: "suspect with lost firstFailure restarts window at now", prev: Record{State: suspect, Reason: v1alpha1.ReasonStatTimeout, Failures: 4, LastClass: hostfs.ProbeStatTimeout},
			class: hostfs.ProbeStatTimeout, now: t0,
			want: failing(suspect, hostfs.ProbeStatTimeout, 5, t0),
		},
		// --- blip / false alarm ---
		{
			name: "suspect ok -> healthy false alarm", prev: failing(suspect, hostfs.ProbeStatTimeout, 2, t0),
			class: hostfs.ProbeOK, now: t0.Add(20 * time.Second),
			want: Record{State: healthy},
			tr:   &Transition{From: suspect, To: healthy, Reason: v1alpha1.ReasonStatTimeout, FalseAlarm: true},
		},
		{
			name: "suspect probe_error -> errored keeps streak", prev: failing(suspect, hostfs.ProbeStatError, 2, t0),
			class: hostfs.ProbeError, now: t0.Add(20 * time.Second),
			want: Record{State: errored, Reason: v1alpha1.ReasonProbeError, FirstFailure: t0, Failures: 2, LastClass: hostfs.ProbeError},
			tr:   &Transition{From: suspect, To: errored, Reason: v1alpha1.ReasonProbeError},
		},
		// --- Broken ---
		{
			name: "broken failure stays broken", prev: failing(broken, hostfs.ProbeWrongFSType, 3, t0),
			class: hostfs.ProbeWrongFSType, now: t0.Add(time.Minute),
			want: failing(broken, hostfs.ProbeWrongFSType, 4, t0),
		},
		{
			name: "broken slow failure inside window stays broken", prev: failing(broken, hostfs.ProbeStatError, 3, t0),
			class: hostfs.ProbeStatTimeout, now: t0.Add(time.Second),
			want: failing(broken, hostfs.ProbeStatTimeout, 4, t0),
		},
		{
			name: "broken ok -> healthy recovery", prev: failing(broken, hostfs.ProbeNotMountpoint, 3, t0),
			class: hostfs.ProbeOK, now: t0.Add(5 * time.Minute),
			want: Record{State: healthy},
			tr:   &Transition{From: broken, To: healthy, Reason: v1alpha1.ReasonNotMountpoint},
		},
		{
			name: "broken probe_error -> errored (never broken)", prev: failing(broken, hostfs.ProbeStatError, 3, t0),
			class: hostfs.ProbeError, now: t0.Add(time.Minute),
			want: Record{State: errored, Reason: v1alpha1.ReasonProbeError, FirstFailure: t0, Failures: 3, LastClass: hostfs.ProbeError},
			tr:   &Transition{From: broken, To: errored, Reason: v1alpha1.ReasonProbeError},
		},
		// --- Errored ---
		{
			name: "errored probe_error stays errored", prev: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
			class: hostfs.ProbeError, now: t0,
			want: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
		},
		{
			name: "errored ok -> healthy (no false alarm)", prev: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
			class: hostfs.ProbeOK, now: t0,
			want: Record{State: healthy},
			tr:   &Transition{From: errored, To: healthy, Reason: v1alpha1.ReasonProbeError},
		},
		{
			name: "errored without streak failure -> suspect n=1", prev: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
			class: hostfs.ProbeStatError, now: t0,
			want: failing(suspect, hostfs.ProbeStatError, 1, t0),
			tr:   &Transition{From: errored, To: suspect, Reason: v1alpha1.ReasonStatError},
		},
		{
			name: "errored with streak continues to broken", prev: Record{State: errored, Reason: v1alpha1.ReasonProbeError, FirstFailure: t0, Failures: 2, LastClass: hostfs.ProbeError},
			class: hostfs.ProbeNotMountpoint, now: t0.Add(30 * time.Second),
			want: failing(broken, hostfs.ProbeNotMountpoint, 3, t0),
			tr:   &Transition{From: errored, To: broken, Reason: v1alpha1.ReasonNotMountpoint},
		},
		{
			name: "errored with negative failures normalised", prev: Record{State: errored, Reason: v1alpha1.ReasonProbeError, FirstFailure: t0, Failures: -1},
			class: hostfs.ProbeError, now: t0,
			want: Record{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := cfg
			if tt.cfg != nil {
				c = *tt.cfg
			}
			got, tr := Step(tt.prev, tt.class, tt.now, c)
			if d := cmp.Diff(tt.want, got); d != "" {
				t.Errorf("record (-want +got):\n%s", d)
			}
			if d := cmp.Diff(tt.tr, tr); d != "" {
				t.Errorf("transition (-want +got):\n%s", d)
			}
			// Convergence: the same input from the same state is stable.
			got2, tr2 := Step(tt.prev, tt.class, tt.now, c)
			if d := cmp.Diff(got, got2); d != "" || cmp.Diff(tr, tr2) != "" {
				t.Errorf("Step not deterministic: %s", d)
			}
		})
	}
}

// TestSequences drives realistic probe sequences end to end.
func TestSequences(t *testing.T) {
	type obs struct {
		class hostfs.ProbeClass
		at    time.Duration
		want  State
	}
	tests := []struct {
		name string
		seq  []obs
		// falseAlarms expected across the sequence.
		falseAlarms int
	}{
		{
			name: "transient blip (S2)",
			seq: []obs{
				{hostfs.ProbeOK, 0, healthy},
				{hostfs.ProbeStatError, 60 * time.Second, suspect},
				{hostfs.ProbeOK, 70 * time.Second, healthy},
			},
			falseAlarms: 1,
		},
		{
			name: "slow server shorter than window (S3)",
			seq: []obs{
				{hostfs.ProbeOK, 0, healthy},
				{hostfs.ProbeStatTimeout, 60 * time.Second, suspect},
				{hostfs.ProbeHung, 70 * time.Second, suspect},
				{hostfs.ProbeHung, 80 * time.Second, suspect},
				{hostfs.ProbeHung, 90 * time.Second, suspect},
				{hostfs.ProbeOK, 100 * time.Second, healthy},
			},
			falseAlarms: 1,
		},
		{
			name: "slow server outlasting window",
			seq: []obs{
				{hostfs.ProbeStatTimeout, 0, suspect},
				{hostfs.ProbeStatTimeout, 60 * time.Second, suspect},
				{hostfs.ProbeHung, 120 * time.Second, suspect},
				{hostfs.ProbeHung, 180 * time.Second, broken},
				{hostfs.ProbeOK, 600 * time.Second, healthy},
			},
		},
		{
			name: "wrong fstype confirms after attempts (S4)",
			seq: []obs{
				{hostfs.ProbeOK, 0, healthy},
				{hostfs.ProbeWrongFSType, 60 * time.Second, suspect},
				{hostfs.ProbeWrongFSType, 70 * time.Second, suspect},
				{hostfs.ProbeWrongFSType, 80 * time.Second, broken},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec Record
			fa := 0
			for i, o := range tt.seq {
				var tr *Transition
				rec, tr = Step(rec, o.class, t0.Add(o.at), cfg)
				if rec.State != o.want {
					t.Fatalf("step %d (%s): state %s, want %s", i, o.class, rec.State, o.want)
				}
				if tr != nil && tr.FalseAlarm {
					fa++
				}
			}
			if fa != tt.falseAlarms {
				t.Fatalf("false alarms = %d, want %d", fa, tt.falseAlarms)
			}
		})
	}
}

// TestRehydration simulates a worker restart after every step: the record is
// persisted through ToStatus, rehydrated with FromStatus, and must produce the
// same decisions as an uninterrupted run.
func TestRehydration(t *testing.T) {
	seq := []hostfs.ProbeClass{
		hostfs.ProbeOK, hostfs.ProbeStatTimeout, hostfs.ProbeError, hostfs.ProbeStatTimeout,
		hostfs.ProbeHung, hostfs.ProbeHung, hostfs.ProbeHung, hostfs.ProbeOK, hostfs.ProbeNotMountpoint,
		hostfs.ProbeNotMountpoint, hostfs.ProbeNotMountpoint,
	}
	var straight, restarted Record
	for i, c := range seq {
		now := t0.Add(time.Duration(i) * time.Minute)
		var tr1, tr2 *Transition
		straight, tr1 = Step(straight, c, now, cfg)
		restarted, tr2 = Step(restarted, c, now, cfg)
		restarted = FromStatus(restarted.ToStatus())
		if d := cmp.Diff(straight, restarted); d != "" {
			t.Fatalf("step %d: rehydrated record differs (-straight +restarted):\n%s", i, d)
		}
		if d := cmp.Diff(tr1, tr2); d != "" {
			t.Fatalf("step %d: transitions differ:\n%s", i, d)
		}
	}
	if straight.State != broken {
		t.Fatalf("final state %s, want Broken", straight.State)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	records := []Record{
		{State: healthy},
		{State: unknown},
		failing(suspect, hostfs.ProbeStatTimeout, 2, t0),
		failing(broken, hostfs.ProbeWrongFSType, 3, t0),
		{State: errored, Reason: v1alpha1.ReasonProbeError, LastClass: hostfs.ProbeError},
		{State: errored, Reason: v1alpha1.ReasonProbeError, FirstFailure: t0, Failures: 2, LastClass: hostfs.ProbeError},
		{State: suspect, Reason: v1alpha1.ReasonStatError, Failures: 1, LastClass: hostfs.ProbeStatError},
	}
	for _, r := range records {
		if d := cmp.Diff(r, FromStatus(r.ToStatus())); d != "" {
			t.Errorf("round trip of %+v (-want +got):\n%s", r, d)
		}
	}
}

func TestToStatus(t *testing.T) {
	h, r, d := Record{}.ToStatus()
	if h != unknown || r != "" || d != nil {
		t.Fatalf("zero record: %s %s %v", h, r, d)
	}
	h, r, d = Record{State: healthy}.ToStatus()
	if h != healthy || r != "" || d != nil {
		t.Fatalf("healthy: %s %s %v", h, r, d)
	}
	h, r, d = failing(broken, hostfs.ProbeHung, 4, t0).ToStatus()
	want := &v1alpha1.Debounce{FirstFailureTime: &metav1.Time{Time: t0}, ConsecutiveFailures: 4, LastClass: v1alpha1.ReasonProbeHung}
	if h != broken || r != v1alpha1.ReasonProbeHung || cmp.Diff(want, d) != "" {
		t.Fatalf("broken: %s %s %s", h, r, cmp.Diff(want, d))
	}
}

func TestFromStatus(t *testing.T) {
	ft := metav1.NewTime(t0)
	deb := &v1alpha1.Debounce{FirstFailureTime: &ft, ConsecutiveFailures: 2, LastClass: v1alpha1.ReasonStatError}
	tests := []struct {
		name string
		h    v1alpha1.Health
		r    v1alpha1.FailureReason
		d    *v1alpha1.Debounce
		want Record
	}{
		{"empty health is unknown", "", "", nil, Record{State: unknown}},
		{"garbage health is unknown", "Weird", v1alpha1.ReasonStatError, deb, Record{State: unknown}},
		{"unknown drops debounce", unknown, v1alpha1.ReasonStatError, deb, Record{State: unknown}},
		{"healthy drops stale debounce", healthy, v1alpha1.ReasonStatError, deb, Record{State: healthy}},
		{"suspect restores debounce", suspect, v1alpha1.ReasonStatError, deb, failing(suspect, hostfs.ProbeStatError, 2, t0)},
		{"suspect without debounce", suspect, v1alpha1.ReasonStatError, nil, Record{State: suspect, Reason: v1alpha1.ReasonStatError}},
		{
			"negative failures clamp", broken, v1alpha1.ReasonNotMountpoint,
			&v1alpha1.Debounce{ConsecutiveFailures: -3, LastClass: v1alpha1.ReasonNotMountpoint},
			Record{State: broken, Reason: v1alpha1.ReasonNotMountpoint, LastClass: hostfs.ProbeNotMountpoint},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if d := cmp.Diff(tt.want, FromStatus(tt.h, tt.r, tt.d)); d != "" {
				t.Fatalf("(-want +got):\n%s", d)
			}
		})
	}
}

// TestRehydratedSuspectDoesNotRetrigger: a worker restart while Broken keeps
// Broken (no new transition, so no re-trigger) on the next failure.
func TestRehydratedBrokenNoNewTransition(t *testing.T) {
	ft := metav1.NewTime(t0)
	rec := FromStatus(broken, v1alpha1.ReasonWrongFSType,
		&v1alpha1.Debounce{FirstFailureTime: &ft, ConsecutiveFailures: 3, LastClass: v1alpha1.ReasonWrongFSType})
	next, tr := Step(rec, hostfs.ProbeWrongFSType, t0.Add(time.Hour), cfg)
	if tr != nil || next.State != broken || next.Failures != 4 {
		t.Fatalf("got %+v %+v", next, tr)
	}
}

func FuzzStep(f *testing.F) {
	classes := []hostfs.ProbeClass{
		hostfs.ProbeOK, hostfs.ProbeNotMountpoint, hostfs.ProbeWrongFSType, hostfs.ProbeStatError,
		hostfs.ProbeStatTimeout, hostfs.ProbeHung, hostfs.ProbeError,
	}
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6}, int32(3), uint16(180))
	f.Fuzz(func(t *testing.T, seq []byte, attempts int32, windowSec uint16) {
		c := Config{RecheckAttempts: attempts, SlowConfirmWindow: time.Duration(windowSec) * time.Second}
		var rec Record
		for i, b := range seq {
			class := classes[int(b)%len(classes)]
			now := t0.Add(time.Duration(i) * 10 * time.Second)
			prev := rec
			var tr *Transition
			rec, tr = Step(rec, class, now, c)
			if (tr != nil) != (prev.State != rec.State && (prev.State != "" || rec.State != unknown)) {
				t.Fatalf("transition %v inconsistent with %s -> %s", tr, prev.State, rec.State)
			}
			if class == hostfs.ProbeError && rec.State != errored {
				t.Fatalf("probe_error led to %s", rec.State)
			}
			if rec.State == broken && prev.State != broken && rec.Failures < max(attempts, 1) {
				t.Fatalf("broken after %d failures (attempts %d)", rec.Failures, attempts)
			}
			if d := cmp.Diff(rec, FromStatus(rec.ToStatus())); d != "" {
				t.Fatalf("round trip:\n%s", d)
			}
		}
	})
}
