package worker

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/health"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/inventory"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

var unitNow = time.Date(2026, 10, 11, 7, 0, 0, 0, time.UTC)

func testVolume() inventory.VolumeView {
	return inventory.VolumeView{
		PVName: "pv", Share: "//s/pv", VolumeHandle: "pv", SmbMountName: "n.pv",
		Globalmount: inventory.GlobalmountView{Path: "/k/gm", Mounted: true, FSType: "cifs", SuperOptions: "soft"},
		PodMounts: []inventory.PodMountView{
			{Namespace: "ns", Pod: "a", UID: "u1", Workload: "Deployment/a", Path: "/k/p1", Mounted: true, FSType: "cifs"},
			{Namespace: "ns", Pod: "b", UID: "u2", Path: "/k/p2", Skip: true, SkipReason: inventory.SkipStartupGrace},
		},
		OrphanDirs: []inventory.OrphanDirView{
			{UID: "o1", Path: "/k/o1", Dir: hostfs.DirFacts{Exists: true, IsDir: true, Empty: true}},
			{UID: "o2", Path: "/k/o2", Dir: hostfs.DirFacts{Exists: true, IsDir: true}},
			{UID: "o3", Path: "/k/o3", Mounted: true},
		},
	}
}

func TestWorkerStatusGlobalmountMounted(t *testing.T) {
	first := unitNow.Add(-time.Minute)
	recs := map[string]health.Record{
		"/k/gm": {State: v1alpha1.HealthSuspect, Reason: v1alpha1.ReasonStatTimeout, Failures: 2, FirstFailure: first, LastClass: hostfs.ProbeStatTimeout},
		"/k/p1": {State: v1alpha1.HealthBroken, Reason: v1alpha1.ReasonWrongFSType, Failures: 3, FirstFailure: first, LastClass: hostfs.ProbeWrongFSType},
		"/k/p2": {State: v1alpha1.HealthBroken, Reason: v1alpha1.ReasonWrongFSType},
	}
	st := workerStatus(testVolume(), recs, 2, unitNow, nil, unitNow)
	if st.Health != v1alpha1.HealthSuspect || st.Reason != v1alpha1.ReasonStatTimeout || st.Debounce == nil ||
		st.Debounce.ConsecutiveFailures != 2 || !st.Debounce.FirstFailureTime.Time.Equal(first) {
		t.Fatalf("top-level must be the globalmount record: %+v %+v", st, st.Debounce)
	}
	if pm := st.PodMounts[0]; pm.Health != v1alpha1.HealthBroken || pm.Reason != v1alpha1.ReasonWrongFSType || pm.Workload != "Deployment/a" {
		t.Fatalf("pod mount: %+v", pm)
	}
	if pm := st.PodMounts[1]; pm.Health != v1alpha1.HealthUnknown || pm.Reason != "" {
		t.Fatalf("skipped pod mount must be Unknown: %+v", pm)
	}
	wantOrphans := []v1alpha1.OrphanPodDir{
		{UID: "o1", Path: "/k/o1", Empty: true}, {UID: "o2", Path: "/k/o2"}, {UID: "o3", Path: "/k/o3", Mounted: true},
	}
	for i, o := range wantOrphans {
		if st.OrphanPodDirs[i] != o {
			t.Fatalf("orphan %d: %+v want %+v", i, st.OrphanPodDirs[i], o)
		}
	}
	if *st.Globalmount != (v1alpha1.GlobalmountStatus{Mounted: true, FSType: "cifs", SuperOptions: "soft", DirExists: true}) {
		t.Fatalf("globalmount: %+v", st.Globalmount)
	}
	if st.PendingQuarantines != 2 || len(st.Conditions) != 2 ||
		st.Conditions[0].Status != metav1.ConditionFalse || st.Conditions[1].Status != metav1.ConditionTrue {
		t.Fatalf("conditions/pending: %+v", st)
	}
}

func TestWorkerStatusGlobalmountNotMounted(t *testing.T) {
	vol := testVolume()
	vol.Globalmount = inventory.GlobalmountView{Path: "/k/gm", Dir: hostfs.DirFacts{Exists: true, IsDir: true}}
	recs := map[string]health.Record{
		"/k/gm": {State: v1alpha1.HealthHealthy}, // stale record must be ignored
		"/k/p1": {State: v1alpha1.HealthBroken, Reason: v1alpha1.ReasonNotMountpoint, Failures: 3},
	}
	st := workerStatus(vol, recs, 0, unitNow, nil, unitNow)
	if st.Health != v1alpha1.HealthBroken || st.Reason != v1alpha1.ReasonNotMountpoint || st.Debounce != nil {
		t.Fatalf("top-level must be the worst pod mount without debounce: %+v", st)
	}
	if *st.Globalmount != (v1alpha1.GlobalmountStatus{DirExists: true}) {
		t.Fatalf("globalmount: %+v", st.Globalmount)
	}
	vol.PodMounts = nil
	if st := workerStatus(vol, recs, 0, unitNow, nil, unitNow); st.Health != v1alpha1.HealthUnknown || st.Conditions[0].Status != metav1.ConditionUnknown {
		t.Fatalf("nothing probed: %+v", st)
	}
}

func TestConditionTransitionTimeKept(t *testing.T) {
	vol := testVolume()
	recs := map[string]health.Record{"/k/gm": {State: v1alpha1.HealthHealthy}}
	old := workerStatus(vol, recs, 0, unitNow, nil, unitNow)
	later := unitNow.Add(time.Hour)
	same := workerStatus(vol, recs, 0, later, old.Conditions, later)
	if !same.Conditions[0].LastTransitionTime.Time.Equal(unitNow) {
		t.Fatalf("unchanged condition must keep its transition time: %v", same.Conditions[0].LastTransitionTime)
	}
	if fingerprint(old) != fingerprint(same) {
		t.Fatal("lastProbeTime and transition times must not change the fingerprint")
	}
	changed := workerStatus(vol, recs, 1, later, old.Conditions, later)
	if !changed.Conditions[1].LastTransitionTime.Time.Equal(later) || fingerprint(changed) == fingerprint(old) {
		t.Fatalf("changed condition: %+v", changed.Conditions[1])
	}
}

func TestFingerprintMatchesRoundTrip(t *testing.T) {
	recs := map[string]health.Record{
		"/k/gm": {State: v1alpha1.HealthSuspect, Reason: v1alpha1.ReasonStatTimeout, Failures: 1,
			FirstFailure: unitNow.Add(123 * time.Millisecond), LastClass: hostfs.ProbeStatTimeout,
			RecoveringSince: unitNow.Add(456 * time.Millisecond)},
	}
	st := workerStatus(testVolume(), recs, 0, unitNow, nil, unitNow)
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back v1alpha1.SmbMountStatus
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	// What the API server returns (second precision, plus control-plane fields).
	back.Remediation = &v1alpha1.ActiveRemediation{Active: true}
	back.Conditions = append(back.Conditions, metav1.Condition{Type: v1alpha1.ConditionRemediating, Status: metav1.ConditionTrue})
	if fingerprint(st) != fingerprint(back) {
		t.Fatalf("fingerprint differs after a round trip:\n%s\n%s", fingerprint(st), fingerprint(back))
	}
}

func TestStatusApplyOnlyWorkerFields(t *testing.T) {
	st := workerStatus(testVolume(), nil, 0, unitNow, nil, unitNow)
	st.Remediation = &v1alpha1.ActiveRemediation{Active: true}
	st.LastRemediation = &v1alpha1.LastRemediation{Name: "x"}
	st.RemediationsLastHour = 3
	st.Conditions = append(st.Conditions, metav1.Condition{Type: v1alpha1.ConditionDegraded})
	b, err := json.Marshal(statusApply(st))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"remediation", "lastRemediation", "remediationsLastHour", "pendingQuarantines"} {
		if _, ok := m[f]; ok {
			t.Fatalf("apply must not contain %s: %s", f, b)
		}
	}
	if conds := m["conditions"].([]any); len(conds) != 2 {
		t.Fatalf("only worker conditions: %s", b)
	}
}

func TestHeartbeatDue(t *testing.T) {
	last := metav1.NewTime(unitNow)
	for _, c := range []struct {
		t    time.Time
		last *metav1.Time
		want bool
	}{
		{unitNow, nil, true},
		{unitNow.Add(4 * time.Minute), &last, false},
		{unitNow.Add(5 * time.Minute), &last, true},
	} {
		if got := heartbeatDue(c.t, c.last, 5*time.Minute); got != c.want {
			t.Errorf("heartbeatDue(%v, %v) = %v", c.t, c.last, got)
		}
	}
}

// concurrencyProber records the maximum number of concurrent probes.
type concurrencyProber struct {
	mu        sync.Mutex
	cur, peak int
	calls     atomic.Int32
}

func (p *concurrencyProber) Probe(_ context.Context, path string, _ hostfs.MountSnapshot) hostfs.ProbeResult {
	p.mu.Lock()
	p.cur++
	p.peak = max(p.peak, p.cur)
	p.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	p.mu.Lock()
	p.cur--
	p.mu.Unlock()
	p.calls.Add(1)
	return hostfs.ProbeResult{Path: path, Class: hostfs.ProbeOK}
}

func (*concurrencyProber) Hung() int { return 0 }

func newUnitWorker(prober hostfs.Prober) *Worker {
	w := &Worker{
		node: "n",
		cfg: config.Worker{CheckInterval: time.Minute, RecheckDelay: 3 * time.Second, RecheckAttempts: 3, ProbeConcurrency: 3, SlowConfirmWindow: time.Minute,
			SlowRecoveryWindow: time.Minute},
		host:  Host{Prober: prober},
		clock: clocktesting.NewFakeClock(unitNow),
		m:     metrics.NewWorker("n"),
	}
	w.Prober = newProber(w)
	return w
}

func TestProbeConcurrencyBounded(t *testing.T) {
	cp := &concurrencyProber{}
	w := newUnitWorker(cp)
	var due []*targetState
	for i := range 20 {
		due = append(due, &targetState{target: inventory.ProbeTarget{Path: "/k/" + string(rune('a'+i))}})
	}
	w.Prober.probe(context.Background(), hostfs.MountSnapshot{}, due)
	if cp.calls.Load() != 20 || cp.peak > 3 || cp.peak < 2 {
		t.Fatalf("calls=%d peak=%d (limit 3)", cp.calls.Load(), cp.peak)
	}
	for _, st := range due {
		if st.probedAt.IsZero() || st.result.Class != hostfs.ProbeOK {
			t.Fatalf("not probed: %+v", st)
		}
	}
}

// TestRecoveringPersistedAndRechecked: a slow streak waiting out
// SLOW_RECOVERY_WINDOW is applied with debounce.recoveringSince (so a worker
// restart keeps it) and probed every RECHECK_DELAY, so a flapping mount is
// caught failing again.
func TestRecoveringPersistedAndRechecked(t *testing.T) {
	w := newUnitWorker(&concurrencyProber{})
	st := &targetState{
		target: inventory.ProbeTarget{Path: "/k/gm", PVName: "pv", Kind: v1alpha1.MountKindGlobalmount},
		rec: health.Record{State: v1alpha1.HealthSuspect, Reason: v1alpha1.ReasonStatTimeout, Failures: 2,
			FirstFailure: unitNow.Add(-10 * time.Second), LastClass: hostfs.ProbeStatTimeout},
		probedAt: unitNow,
		result:   hostfs.ProbeResult{Class: hostfs.ProbeOK},
	}
	if tr := w.Prober.step(logr.Discard(), st, indexView(inventory.NodeView{})); tr != nil {
		t.Fatalf("recovering must not transition: %+v", tr)
	}
	if !st.rec.Recovering() || st.rec.State != v1alpha1.HealthSuspect {
		t.Fatalf("record: %+v", st.rec)
	}
	if !st.nextDue.Equal(unitNow.Add(3 * time.Second)) {
		t.Fatalf("recovering mounts are rechecked after RECHECK_DELAY: %v", st.nextDue)
	}
	if v := metricValue(w.m.FalseAlarms.WithLabelValues("globalmount", "stat_timeout")); v != 0 {
		t.Fatalf("false alarms = %v", v)
	}

	b, err := json.Marshal(statusApply(workerStatus(testVolume(), map[string]health.Record{"/k/gm": st.rec}, 0, unitNow, nil, unitNow)))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Debounce struct {
			RecoveringSince *metav1.Time `json:"recoveringSince"`
		} `json:"debounce"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Debounce.RecoveringSince == nil || !m.Debounce.RecoveringSince.Time.Equal(unitNow.Truncate(time.Second)) {
		t.Fatalf("apply must carry debounce.recoveringSince: %s", b)
	}
}

func TestStepCadence(t *testing.T) {
	w := newUnitWorker(&concurrencyProber{})
	st := &targetState{
		target: inventory.ProbeTarget{Path: "/k/gm", PVName: "pv", Kind: v1alpha1.MountKindGlobalmount},
		rec:    health.Record{State: v1alpha1.HealthUnknown}, probedAt: unitNow,
		result: hostfs.ProbeResult{Class: hostfs.ProbeStatError},
	}
	idx := indexView(inventory.NodeView{})
	if tr := w.Prober.step(logr.Discard(), st, idx); tr == nil || tr.to != v1alpha1.HealthSuspect {
		t.Fatalf("transition: %+v", tr)
	}
	if !st.nextDue.Equal(unitNow.Add(3 * time.Second)) {
		t.Fatalf("suspect mounts are rechecked after RECHECK_DELAY: %v", st.nextDue)
	}
	st.result = hostfs.ProbeResult{Class: hostfs.ProbeOK}
	if tr := w.Prober.step(logr.Discard(), st, idx); tr == nil || tr.from != v1alpha1.HealthSuspect {
		t.Fatalf("transition: %+v", tr)
	}
	if !st.nextDue.Equal(unitNow.Add(time.Minute)) {
		t.Fatalf("healthy mounts are checked every CHECK_INTERVAL: %v", st.nextDue)
	}
	if v := metricValue(w.m.FalseAlarms.WithLabelValues("globalmount", "stat_error")); v != 1 {
		t.Fatalf("false alarms = %v", v)
	}
}
