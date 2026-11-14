package config

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func parse(t *testing.T, args []string, kv map[string]string) (Config, error) {
	t.Helper()
	return Parse(NewFlagSet("smb-mount-healer"), args, envOf(kv))
}

func TestModeSelection(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Mode
		wantErr error
	}{
		{name: "none", args: nil, wantErr: ErrUsage},
		{name: "both", args: []string{"--controlplane", "--worker"}, wantErr: ErrUsage},
		{name: "controlplane", args: []string{"--controlplane"}, want: ModeControlPlane},
		{name: "worker", args: []string{"--worker"}, env: map[string]string{"NODE_NAME": "n1"}, want: ModeWorker},
		{name: "worker dry-run", args: []string{"--worker", "--dry-run"}, env: map[string]string{"NODE_NAME": "n1"}, wantErr: ErrUsage},
		{name: "version", args: []string{"--version"}, want: ModeVersion},
		{name: "unknown flag", args: []string{"--nope"}, wantErr: ErrUsage},
		{name: "positional", args: []string{"--worker", "extra"}, wantErr: ErrUsage},
		{name: "worker without node", args: []string{"--worker"}, wantErr: ErrInvalid},
		{name: "help", args: []string{"-h"}, wantErr: flag.ErrHelp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parse(t, tt.args, tt.env)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mode != tt.want {
				t.Fatalf("mode = %q, want %q", cfg.Mode, tt.want)
			}
		})
	}
}

func TestHiddenProbe(t *testing.T) {
	cfg, err := parse(t, []string{"--probe=stat", "/var/lib/kubelet/x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Mode: ModeProbe, Probe: Probe{Op: "stat", Args: []string{"/var/lib/kubelet/x"}}}
	if d := cmp.Diff(want, cfg); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
	var buf bytes.Buffer
	Usage(&buf, func() *flag.FlagSet { fs := NewFlagSet("x"); bindFlags(fs, &flagValues{}); return fs }())
	if strings.Contains(buf.String(), "--probe\n") || strings.Contains(buf.String(), "internal:") {
		t.Fatalf("usage mentions hidden flag:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "--controlplane") {
		t.Fatalf("usage misses --controlplane:\n%s", buf.String())
	}
}

func TestWorkerDefaults(t *testing.T) {
	cfg, err := parse(t, []string{"--worker"}, map[string]string{"NODE_NAME": "n1"})
	if err != nil {
		t.Fatal(err)
	}
	want := Worker{
		NodeName:                 "n1",
		KubeletRoot:              "/var/lib/kubelet",
		ExpectedFSTypes:          []string{"cifs", "smb3"},
		CheckInterval:            60 * time.Second,
		RecheckAttempts:          3,
		RecheckDelay:             3 * time.Second,
		StatTimeout:              5 * time.Second,
		StartupGrace:             10 * time.Second,
		PodStartupGrace:          60 * time.Second,
		SlowConfirmWindow:        3 * time.Minute,
		SlowRecoveryWindow:       2 * time.Minute,
		ProbeConcurrency:         4,
		StatusHeartbeat:          5 * time.Minute,
		QuarantineDir:            "/var/lib/kubelet/smb-mount-healer-quarantine",
		QuarantineScanInterval:   5 * time.Minute,
		QuarantineScanMaxEntries: 100000,
		QuarantineScanTimeBudget: 10 * time.Second,
	}
	if d := cmp.Diff(want, cfg.Worker); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}
	if cfg.Common != (Common{MetricsBindAddress: ":8080", HealthProbeBindAddress: ":8081", CSIDriverName: "smb.csi.k8s.io"}) {
		t.Fatalf("common = %+v", cfg.Common)
	}
}

func TestWorkerOverrides(t *testing.T) {
	cfg, err := parse(t, []string{"--worker", "--metrics-bind-address=:9090"}, map[string]string{
		"NODE_NAME":                   "n1",
		"KUBELET_ROOT":                "/k",
		"CSI_DRIVER_NAME":             "x.csi",
		"EXPECTED_FSTYPES":            " cifs ",
		"CHECK_INTERVAL":              "300",
		"RECHECK_ATTEMPTS":            "5",
		"RECHECK_DELAY":               "1.5",
		"STAT_TIMEOUT":                "2s",
		"STARTUP_GRACE":               "0",
		"POD_STARTUP_GRACE":           "2m",
		"SLOW_CONFIRM_WINDOW":         "1m30s",
		"SLOW_RECOVERY_WINDOW":        "45s",
		"PROBE_CONCURRENCY":           "8",
		"STATUS_HEARTBEAT":            "10m",
		"QUARANTINE_SCAN_INTERVAL":    "60",
		"QUARANTINE_SCAN_MAX_ENTRIES": "5",
		"QUARANTINE_SCAN_TIME_BUDGET": "1s",
	})
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Worker
	if w.KubeletRoot != "/k" || w.QuarantineDir != "/k/smb-mount-healer-quarantine" || cfg.Common.CSIDriverName != "x.csi" ||
		cfg.Common.MetricsBindAddress != ":9090" {
		t.Fatalf("paths/common: %+v %+v", w, cfg.Common)
	}
	if d := cmp.Diff([]string{"cifs"}, w.ExpectedFSTypes); d != "" {
		t.Fatal(d)
	}
	checks := map[string][2]time.Duration{
		"CHECK_INTERVAL":      {w.CheckInterval, 300 * time.Second},
		"RECHECK_DELAY":       {w.RecheckDelay, 1500 * time.Millisecond},
		"STAT_TIMEOUT":        {w.StatTimeout, 2 * time.Second},
		"STARTUP_GRACE":       {w.StartupGrace, 0},
		"POD_STARTUP_GRACE":   {w.PodStartupGrace, 2 * time.Minute},
		"SLOW_CONFIRM_WINDOW": {w.SlowConfirmWindow, 90 * time.Second},
		"SLOW_RECOVERY_WIN":   {w.SlowRecoveryWindow, 45 * time.Second},
		"STATUS_HEARTBEAT":    {w.StatusHeartbeat, 10 * time.Minute},
		"QUARANTINE_SCAN_INT": {w.QuarantineScanInterval, time.Minute},
		"QUARANTINE_SCAN_TB":  {w.QuarantineScanTimeBudget, time.Second},
	}
	for k, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %s, want %s", k, c[0], c[1])
		}
	}
	if w.RecheckAttempts != 5 || w.ProbeConcurrency != 8 || w.QuarantineScanMaxEntries != 5 {
		t.Fatalf("ints: %+v", w)
	}
}

func TestControlPlaneDefaultsAndOverrides(t *testing.T) {
	cfg, err := parse(t, []string{"--controlplane"}, map[string]string{"POD_NAMESPACE": "smb"})
	if err != nil {
		t.Fatal(err)
	}
	want := ControlPlane{
		LeaderElection:              true,
		LeaderElectionNamespace:     "smb",
		LeaderElectionID:            "smb-mount-healer-controlplane",
		CSINodeDriverNamespace:      "kube-system",
		CSINodeDriverLabelSelector:  "app=csi-smb-node",
		PollInterval:                3 * time.Second,
		PlanningProbeWait:           15 * time.Second,
		ScaleDownWait:               120 * time.Second,
		UnstageWait:                 30 * time.Second,
		QuarantineUnstageWait:       300 * time.Second,
		DriverRestartWait:           90 * time.Second,
		VerifyWait:                  90 * time.Second,
		VerifyNoPodGrace:            30 * time.Second,
		EpisodeMaxDuration:          30 * time.Minute,
		BackoffBase:                 5 * time.Minute,
		BackoffMax:                  time.Hour,
		RemediationRetention:        24 * time.Hour,
		RemediationRetainMinPerNode: 10,
	}
	if d := cmp.Diff(want, cfg.ControlPlane); d != "" {
		t.Fatalf("(-want +got)\n%s", d)
	}

	cfg, err = parse(t, []string{"--controlplane", "--dry-run", "--leader-elect=false"}, map[string]string{
		"SCALE_DOWN_WAIT":           "10",
		"UNSTAGE_WAIT":              "20s",
		"QUARANTINE_UNSTAGE_WAIT":   "7",
		"DRIVER_RESTART_WAIT":       "8",
		"VERIFY_WAIT":               "9",
		"LEADER_ELECTION_NAMESPACE": "le",
		"POD_NAMESPACE":             "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	cp := cfg.ControlPlane
	if !cp.DryRun || cp.LeaderElection || cp.LeaderElectionNamespace != "le" {
		t.Fatalf("flags: %+v", cp)
	}
	if cp.ScaleDownWait != 10*time.Second || cp.UnstageWait != 20*time.Second || cp.QuarantineUnstageWait != 7*time.Second ||
		cp.DriverRestartWait != 8*time.Second || cp.VerifyWait != 9*time.Second {
		t.Fatalf("durations: %+v", cp)
	}

	cfg, err = parse(t, []string{"--controlplane"}, map[string]string{"DRY_RUN": "true"})
	if err != nil || !cfg.ControlPlane.DryRun {
		t.Fatalf("DRY_RUN env: %v %+v", err, cfg.ControlPlane)
	}
}

func TestValidationErrors(t *testing.T) {
	worker := func(kv map[string]string) map[string]string {
		kv["NODE_NAME"] = "n1"
		return kv
	}
	tests := []struct {
		name string
		args []string
		env  map[string]string
		msg  string
	}{
		{"bad duration", []string{"--worker"}, worker(map[string]string{"CHECK_INTERVAL": "soon"}), "CHECK_INTERVAL"},
		{"zero duration", []string{"--worker"}, worker(map[string]string{"RECHECK_DELAY": "0"}), "RECHECK_DELAY must be positive"},
		{"negative grace", []string{"--worker"}, worker(map[string]string{"POD_STARTUP_GRACE": "-1"}), "must not be negative"},
		{"negative slow recovery", []string{"--worker"}, worker(map[string]string{"SLOW_RECOVERY_WINDOW": "-1s"}), "SLOW_RECOVERY_WINDOW must not be negative"},
		{"bad int", []string{"--worker"}, worker(map[string]string{"RECHECK_ATTEMPTS": "x"}), "RECHECK_ATTEMPTS"},
		{"zero attempts", []string{"--worker"}, worker(map[string]string{"RECHECK_ATTEMPTS": "0"}), "RECHECK_ATTEMPTS must be >= 1"},
		{"zero concurrency", []string{"--worker"}, worker(map[string]string{"PROBE_CONCURRENCY": "0"}), "PROBE_CONCURRENCY"},
		{"zero max entries", []string{"--worker"}, worker(map[string]string{"QUARANTINE_SCAN_MAX_ENTRIES": "0"}), "QUARANTINE_SCAN_MAX_ENTRIES"},
		{"relative root", []string{"--worker"}, worker(map[string]string{"KUBELET_ROOT": "kubelet"}), "KUBELET_ROOT must be absolute"},
		{"relative quarantine", []string{"--worker"}, worker(map[string]string{"QUARANTINE_DIR": "q"}), "QUARANTINE_DIR must be absolute"},
		{"no fstypes", []string{"--worker"}, worker(map[string]string{"EXPECTED_FSTYPES": "  "}), ""}, // blank means default
		{"stat >= interval", []string{"--worker"}, worker(map[string]string{"STAT_TIMEOUT": "60"}), "STAT_TIMEOUT"},
		{"bad selector", []string{"--controlplane"}, map[string]string{"CSI_NODE_DRIVER_LABEL_SELECTOR": "a in (b"}, "CSI_NODE_DRIVER_LABEL_SELECTOR"},
		{"backoff order", []string{"--controlplane"}, map[string]string{"BACKOFF_BASE": "2h"}, "BACKOFF_BASE"},
		{"bad bool", []string{"--controlplane"}, map[string]string{"DRY_RUN": "maybe"}, "DRY_RUN"},
		{"zero poll", []string{"--controlplane"}, map[string]string{"POLL_INTERVAL": "0s"}, "POLL_INTERVAL must be positive"},
		{"negative retain", []string{"--controlplane"}, map[string]string{"REMEDIATION_RETAIN_MIN_PER_NODE": "-1"}, "REMEDIATION_RETAIN_MIN_PER_NODE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse(t, tt.args, tt.env)
			if tt.msg == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tt.msg)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"300": 300 * time.Second, "0.5": 500 * time.Millisecond, "5m": 5 * time.Minute, "1h": time.Hour,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if d, err := ParseDuration(" 2h "); err != nil || d != 2*time.Hour {
		t.Errorf("whitespace not trimmed: %v %v", d, err)
	}
	if _, err := ParseDuration("5 minutes"); err == nil {
		t.Error("expected error")
	}
}
