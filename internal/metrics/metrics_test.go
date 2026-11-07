package metrics

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

func gatherText(t *testing.T, r *prometheus.Registry) (string, []*dto.MetricFamily) {
	t.Helper()
	mfs, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String(), mfs
}

func labelNames(m *dto.Metric) []string {
	var out []string
	for _, lp := range m.GetLabel() {
		out = append(out, lp.GetName())
	}
	slices.Sort(out)
	return out
}

func sorted(ls ...[]string) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l...)
	}
	slices.Sort(out)
	return out
}

func TestWorkerContract(t *testing.T) {
	r := prometheus.NewRegistry()
	w := NewWorker("n1")
	if err := w.Register(r); err != nil {
		t.Fatal(err)
	}
	if err := RegisterBuildInfo(r, ComponentWorker, "1.2.3", "abc"); err != nil {
		t.Fatal(err)
	}
	vals := func(ls []string) []string { return slices.Repeat([]string{"x"}, len(ls)) }
	w.MountChecks.WithLabelValues(vals(MountChecksTotalLabels)...).Inc()
	w.MountCheckDuration.WithLabelValues(vals(MountCheckDurationSecondsLabels)...).Observe(0.1)
	SetEnum(w.MountStatus, prometheus.Labels{LabelKind: "pod", LabelPV: "pv", LabelShare: "//s/x", LabelWorkloadNamespace: "ns", LabelWorkload: "app"},
		LabelStatus, []string{"Healthy", "Broken"}, "Broken")
	w.HealthTransitions.WithLabelValues(vals(HealthTransitionsTotalLabels)...).Inc()
	w.FalseAlarms.WithLabelValues(vals(FalseAlarmsTotalLabels)...).Inc()
	w.Quarantines.WithLabelValues("pv").Set(1)
	w.QuarantineBytes.WithLabelValues("pv").Set(1)
	w.QuarantineOldestTimestamp.WithLabelValues("pv").Set(1)
	w.QuarantineScans.WithLabelValues("success").Inc()
	w.HostActions.WithLabelValues(vals(HostActionsTotalLabels)...).Inc()
	w.SmbMountStatusWrites.WithLabelValues("success").Inc()

	text, mfs := gatherText(t, r)
	want := map[string][]string{
		MountChecksTotal:                 MountChecksTotalLabels,
		MountCheckDurationSeconds:        MountCheckDurationSecondsLabels,
		MountStatus:                      MountStatusLabels,
		HealthTransitionsTotal:           HealthTransitionsTotalLabels,
		FalseAlarmsTotal:                 FalseAlarmsTotalLabels,
		ProbesHung:                       nil,
		UnknownGlobalmounts:              nil,
		Quarantines:                      QuarantinesLabels,
		QuarantineBytes:                  QuarantineBytesLabels,
		QuarantineOldestTimestampSeconds: QuarantineOldestTimestampSecondsLabels,
		QuarantineScansTotal:             QuarantineScansTotalLabels,
		HostActionsTotal:                 HostActionsTotalLabels,
		SmbMountStatusWritesTotal:        SmbMountStatusWritesTotalLabels,
	}
	checkFamilies(t, mfs, want, []string{LabelComponent, LabelNode})
	if !strings.Contains(text, `smb_mount_healer_mount_status{component="worker",kind="pod",node="n1",pv="pv",share="//s/x",status="Broken",workload="app",workload_namespace="ns"} 1`) {
		t.Fatalf("enum gauge not rendered as expected:\n%s", text)
	}
	if !strings.Contains(text, `smb_mount_healer_build_info{component="worker",mode="worker",revision="abc",version="1.2.3"} 1`) {
		t.Fatalf("build_info missing:\n%s", text)
	}
	DeleteEnum(w.MountStatus, prometheus.Labels{LabelPV: "pv"})
	if text, _ := gatherText(t, r); strings.Contains(text, MountStatus+"{") {
		t.Fatal("DeleteEnum left series behind")
	}
}

func TestControlPlaneContract(t *testing.T) {
	r := prometheus.NewRegistry()
	c := NewControlPlane()
	if err := c.Register(r); err != nil {
		t.Fatal(err)
	}
	if err := c.Register(r); err == nil {
		t.Fatal("double registration must fail")
	}
	vals := func(ls []string) []string { return slices.Repeat([]string{"x"}, len(ls)) }
	c.RemediationStage.WithLabelValues(vals(RemediationStageLabels)...).Set(1)
	c.RemediationTransitions.WithLabelValues(vals(RemediationTransitionsTotalLabels)...).Inc()
	c.RemediationStageDuration.WithLabelValues(vals(RemediationStageDurationSecondsLabels)...).Observe(1)
	c.RemediationsStarted.WithLabelValues(vals(RemediationsStartedTotalLabels)...).Inc()
	c.Remediations.WithLabelValues(vals(RemediationsTotalLabels)...).Inc()
	c.RemediationDuration.WithLabelValues(vals(RemediationDurationSecondsLabels)...).Observe(1)
	c.RemediationsResumed.WithLabelValues(vals(RemediationsResumedTotalLabels)...).Inc()
	c.RemediationSuppressed.WithLabelValues(vals(RemediationSuppressedTotalLabels)...).Inc()
	c.WorkloadActions.WithLabelValues(vals(WorkloadActionsTotalLabels)...).Inc()
	c.WorkloadVerifications.WithLabelValues(vals(WorkloadVerificationsTotalLabels)...).Inc()
	c.SmbMounts.WithLabelValues("Healthy").Set(3)

	text, mfs := gatherText(t, r)
	if !strings.Contains(text, `smb_mount_healer_dry_run{component="controlplane"} 0`) {
		t.Fatalf("dry_run must default to 0 (enforcing):\n%s", text)
	}
	c.SetDryRun(true)
	if text, _ := gatherText(t, r); !strings.Contains(text, `smb_mount_healer_dry_run{component="controlplane"} 1`) {
		t.Fatalf("dry_run not set to 1:\n%s", text)
	}
	c.SetDryRun(false)
	if text, _ := gatherText(t, r); !strings.Contains(text, `smb_mount_healer_dry_run{component="controlplane"} 0`) {
		t.Fatalf("dry_run not reset to 0:\n%s", text)
	}
	checkFamilies(t, mfs, map[string][]string{
		RemediationStage:                RemediationStageLabels,
		RemediationTransitionsTotal:     RemediationTransitionsTotalLabels,
		RemediationStageDurationSeconds: RemediationStageDurationSecondsLabels,
		RemediationsStartedTotal:        RemediationsStartedTotalLabels,
		RemediationsTotal:               RemediationsTotalLabels,
		RemediationDurationSeconds:      RemediationDurationSecondsLabels,
		RemediationsResumedTotal:        RemediationsResumedTotalLabels,
		RemediationSuppressedTotal:      RemediationSuppressedTotalLabels,
		WorkloadActionsTotal:            WorkloadActionsTotalLabels,
		WorkloadVerificationsTotal:      WorkloadVerificationsTotalLabels,
		SmbMounts:                       SmbMountsLabels,
		DryRun:                          nil,
	}, []string{LabelComponent})
}

func checkFamilies(t *testing.T, mfs []*dto.MetricFamily, want map[string][]string, constLabels []string) {
	t.Helper()
	got := map[string]bool{}
	for _, mf := range mfs {
		name := mf.GetName()
		if !strings.HasPrefix(name, Namespace+"_") {
			t.Errorf("%s: missing prefix", name)
		}
		labels, ok := want[name]
		if !ok {
			continue
		}
		got[name] = true
		for _, m := range mf.GetMetric() {
			if g, w := labelNames(m), sorted(labels, constLabels); !slices.Equal(g, w) {
				t.Errorf("%s labels = %v, want %v", name, g, w)
			}
		}
		for _, forbidden := range []string{"namespace", "pod"} {
			if slices.Contains(labels, forbidden) {
				t.Errorf("%s uses forbidden label %q", name, forbidden)
			}
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s not exported", name)
		}
	}
}
