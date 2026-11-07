// Package production_test keeps the Helm chart in production/smb-mount-healer
// consistent with the binary: the chart's default values must equal the
// built-in defaults of internal/config, every tunable must map to an
// environment variable the binary actually reads, and the templates must pass
// each value to that variable.
package production_test

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/marcherschel/smb-mount-healer/internal/config"
)

const chartDir = "smb-mount-healer"

// setting maps one chart value (dotted path in values.yaml) to the
// environment variable the chart renders it into.
type setting struct {
	path string
	env  string
	mode config.Mode // which binary mode reads it
	// sentinel is a valid non-default value for string settings (durations
	// and integers derive theirs from the default).
	sentinel string
}

var settings = []setting{
	{path: "csi.driverName", env: "CSI_DRIVER_NAME", mode: config.ModeControlPlane, sentinel: "x.csi.example.com"},
	{path: "csi.driverName", env: "CSI_DRIVER_NAME", mode: config.ModeWorker, sentinel: "x.csi.example.com"},
	{path: "csi.nodeDriver.namespace", env: "CSI_NODE_DRIVER_NAMESPACE", mode: config.ModeControlPlane, sentinel: "csi"},
	{path: "csi.nodeDriver.labelSelector", env: "CSI_NODE_DRIVER_LABEL_SELECTOR", mode: config.ModeControlPlane, sentinel: "app=other"},
	{path: "kubeletRoot", env: "KUBELET_ROOT", mode: config.ModeWorker, sentinel: "/var/lib/k0s/kubelet"},
	{path: "quarantineDir", env: "QUARANTINE_DIR", mode: config.ModeWorker, sentinel: "/var/lib/kubelet/q"},

	{path: "controlplane.remediation.scaleDownWait", env: "SCALE_DOWN_WAIT", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.unstageWait", env: "UNSTAGE_WAIT", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.quarantineUnstageWait", env: "QUARANTINE_UNSTAGE_WAIT", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.driverRestartWait", env: "DRIVER_RESTART_WAIT", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.verifyWait", env: "VERIFY_WAIT", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.verifyNoPodGrace", env: "VERIFY_NO_POD_GRACE", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.episodeMaxDuration", env: "EPISODE_MAX_DURATION", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.backoffBase", env: "BACKOFF_BASE", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.backoffMax", env: "BACKOFF_MAX", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.pollInterval", env: "POLL_INTERVAL", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.planningProbeWait", env: "PLANNING_PROBE_WAIT", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.retention", env: "REMEDIATION_RETENTION", mode: config.ModeControlPlane},
	{path: "controlplane.remediation.retainMinPerNode", env: "REMEDIATION_RETAIN_MIN_PER_NODE", mode: config.ModeControlPlane},

	{path: "worker.probing.checkInterval", env: "CHECK_INTERVAL", mode: config.ModeWorker},
	{path: "worker.probing.recheckAttempts", env: "RECHECK_ATTEMPTS", mode: config.ModeWorker},
	{path: "worker.probing.recheckDelay", env: "RECHECK_DELAY", mode: config.ModeWorker},
	{path: "worker.probing.statTimeout", env: "STAT_TIMEOUT", mode: config.ModeWorker},
	{path: "worker.probing.slowConfirmWindow", env: "SLOW_CONFIRM_WINDOW", mode: config.ModeWorker},
	{path: "worker.probing.podStartupGrace", env: "POD_STARTUP_GRACE", mode: config.ModeWorker},
	{path: "worker.probing.startupGrace", env: "STARTUP_GRACE", mode: config.ModeWorker},
	{path: "worker.probing.probeConcurrency", env: "PROBE_CONCURRENCY", mode: config.ModeWorker},
	{path: "worker.probing.expectedFsTypes", env: "EXPECTED_FSTYPES", mode: config.ModeWorker, sentinel: "cifs"},
	{path: "worker.probing.statusHeartbeat", env: "STATUS_HEARTBEAT", mode: config.ModeWorker},
	{path: "worker.quarantineScan.interval", env: "QUARANTINE_SCAN_INTERVAL", mode: config.ModeWorker},
	{path: "worker.quarantineScan.maxEntries", env: "QUARANTINE_SCAN_MAX_ENTRIES", mode: config.ModeWorker},
	{path: "worker.quarantineScan.timeBudget", env: "QUARANTINE_SCAN_TIME_BUDGET", mode: config.ModeWorker},
}

// tunableRoots are the values subtrees that must be fully covered by settings.
var tunableRoots = []string{"controlplane.remediation", "worker.probing", "worker.quarantineScan"}

// templates holds, per mode, the template that renders that mode's env.
var templates = map[config.Mode]string{
	config.ModeControlPlane: chartDir + "/templates/controlplane/deployment.yaml",
	config.ModeWorker:       chartDir + "/templates/worker/daemonset.yaml",
}

func loadValues(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(chartDir + "/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func lookup(t *testing.T, values map[string]any, path string) any {
	t.Helper()
	var cur any = values
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("values.yaml: %s: %q is not below a map", path, k)
		}
		if cur, ok = m[k]; !ok {
			t.Fatalf("values.yaml: %s is missing", path)
		}
	}
	return cur
}

// envValue renders a values.yaml scalar the way the chart does.
func envValue(t *testing.T, path string, v any) string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return x
	case float64: // JSON numbers; the chart renders integers with `int64`
		if x != math.Trunc(x) {
			t.Fatalf("%s: %v is not an integer", path, x)
		}
		return strconv.FormatInt(int64(x), 10)
	default:
		t.Fatalf("%s: unexpected type %T", path, v)
		return ""
	}
}

func modeArgs(m config.Mode) []string { return []string{"--" + string(m)} }

func parse(t *testing.T, m config.Mode, env map[string]string) config.Config {
	t.Helper()
	full := map[string]string{"NODE_NAME": "n1"}
	for k, v := range env {
		full[k] = v
	}
	cfg, err := config.Parse(config.NewFlagSet("smb-mount-healer"), modeArgs(m), func(k string) string { return full[k] })
	if err != nil {
		t.Fatalf("%s with %v: %v", m, env, err)
	}
	return cfg
}

// TestChartDefaultsMatchBinary: the config the binary builds from the env the
// chart renders with its default values equals the binary's own defaults.
func TestChartDefaultsMatchBinary(t *testing.T) {
	values := loadValues(t)
	for _, m := range []config.Mode{config.ModeControlPlane, config.ModeWorker} {
		env := map[string]string{}
		for _, s := range settings {
			if s.mode != m {
				continue
			}
			if v := envValue(t, s.path, lookup(t, values, s.path)); v != "" {
				env[s.env] = v
			}
		}
		if d := cmp.Diff(parse(t, m, nil), parse(t, m, env)); d != "" {
			t.Errorf("%s: chart defaults differ from internal/config defaults (-binary +chart):\n%s", m, d)
		}
	}
}

// TestChartSettingsAreRead: every mapped variable is read by the binary (a
// non-default value changes the parsed config), so a typo in an env name
// cannot hide behind equal defaults.
func TestChartSettingsAreRead(t *testing.T) {
	values := loadValues(t)
	for _, s := range settings {
		t.Run(s.env+"/"+string(s.mode), func(t *testing.T) {
			val := s.sentinel
			if val == "" {
				val = bump(t, s.path, lookup(t, values, s.path))
			}
			if d := cmp.Diff(parse(t, s.mode, nil), parse(t, s.mode, map[string]string{s.env: val})); d == "" {
				t.Errorf("%s=%s did not change the %s config: the binary does not read %s", s.env, val, s.mode, s.env)
			}
		})
	}
}

// bump returns a valid non-default value: durations get one second shorter
// (keeps STAT_TIMEOUT < CHECK_INTERVAL and BACKOFF_BASE <= BACKOFF_MAX valid
// for the bumped side; zero becomes 1s), integers one more.
func bump(t *testing.T, path string, v any) string {
	t.Helper()
	if f, ok := v.(float64); ok {
		return strconv.FormatInt(int64(f)+1, 10)
	}
	d, err := config.ParseDuration(envValue(t, path, v))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if d > time.Second {
		return (d - time.Second).String()
	}
	return (d + time.Second).String()
}

// TestChartCoversEveryTunable: every key under the tunable subtrees of
// values.yaml is mapped (no value the chart silently drops).
func TestChartCoversEveryTunable(t *testing.T) {
	values := loadValues(t)
	mapped := map[string]bool{}
	for _, s := range settings {
		mapped[s.path] = true
	}
	for _, root := range tunableRoots {
		sub, ok := lookup(t, values, root).(map[string]any)
		if !ok {
			t.Fatalf("%s is not a map", root)
		}
		for k := range sub {
			if !mapped[root+"."+k] {
				t.Errorf("values.yaml %s.%s has no environment variable mapping in this test (and probably none in the chart)", root, k)
			}
		}
	}
}

// TestChartTemplatesRenderSettings: each template sets each of its variables
// from the mapped value.
func TestChartTemplatesRenderSettings(t *testing.T) {
	for m, file := range templates {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		tpl := string(raw)
		var names []string
		for _, s := range settings {
			if s.mode != m {
				continue
			}
			names = append(names, s.env)
			key := s.path[strings.LastIndex(s.path, ".")+1:]
			re := regexp.MustCompile(fmt.Sprintf(`- name: %s\n\s+value: \{\{ (\.\S*\.|\$\w+\.)%s[ |]`, s.env, key))
			if s.path == "quarantineDir" { // rendered inside `with .Values.quarantineDir`
				re = regexp.MustCompile(`with \.Values\.quarantineDir \}\}\n\s+- name: QUARANTINE_DIR\n\s+value: \{\{ \. `)
			}
			if !re.MatchString(tpl) {
				t.Errorf("%s: env %s is not rendered from %s (want %s)", file, s.env, s.path, re)
			}
		}
		if len(names) == 0 {
			t.Fatalf("%s: no settings", file)
		}
	}
}
