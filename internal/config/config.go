package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
)

// Mode is the process mode.
type Mode string

const (
	ModeControlPlane Mode = "controlplane"
	ModeWorker       Mode = "worker"
	// ModeProbe is the hidden self-exec child mode (internal/probe).
	ModeProbe Mode = "probe"
	// ModeVersion prints the version and exits.
	ModeVersion Mode = "version"
)

var (
	// ErrUsage marks command-line errors (main exits 2 and prints usage).
	ErrUsage = errors.New("usage error")
	// ErrInvalid marks configuration validation errors (main exits 2).
	ErrInvalid = errors.New("invalid configuration")
)

// Config is the parsed configuration. Only the section matching Mode is filled.
type Config struct {
	Mode         Mode
	Common       Common
	Worker       Worker
	ControlPlane ControlPlane
	Probe        Probe
}

// Common applies to both long-running modes.
type Common struct {
	MetricsBindAddress     string // --metrics-bind-address (":8080")
	HealthProbeBindAddress string // --health-probe-bind-address (":8081")
	CSIDriverName          string // CSI_DRIVER_NAME (smb.csi.k8s.io)
}

// Worker is the --worker configuration.
type Worker struct {
	NodeName                 string        // NODE_NAME (required)
	KubeletRoot              string        // KUBELET_ROOT (/var/lib/kubelet)
	ExpectedFSTypes          []string      // EXPECTED_FSTYPES ("cifs smb3")
	CheckInterval            time.Duration // CHECK_INTERVAL (60s)
	RecheckAttempts          int32         // RECHECK_ATTEMPTS (3)
	RecheckDelay             time.Duration // RECHECK_DELAY (3s)
	StatTimeout              time.Duration // STAT_TIMEOUT (5s)
	StartupGrace             time.Duration // STARTUP_GRACE (10s)
	PodStartupGrace          time.Duration // POD_STARTUP_GRACE (60s)
	SlowConfirmWindow        time.Duration // SLOW_CONFIRM_WINDOW (3m)
	ProbeConcurrency         int           // PROBE_CONCURRENCY (4)
	StatusHeartbeat          time.Duration // STATUS_HEARTBEAT (5m)
	QuarantineDir            string        // QUARANTINE_DIR ($KUBELET_ROOT/smb-mount-healer-quarantine)
	QuarantineScanInterval   time.Duration // QUARANTINE_SCAN_INTERVAL (5m)
	QuarantineScanMaxEntries int64         // QUARANTINE_SCAN_MAX_ENTRIES (100000)
	QuarantineScanTimeBudget time.Duration // QUARANTINE_SCAN_TIME_BUDGET (10s)
}

// ControlPlane is the --controlplane configuration.
type ControlPlane struct {
	DryRun                      bool          // --dry-run or DRY_RUN
	LeaderElection              bool          // --leader-elect (true)
	LeaderElectionNamespace     string        // LEADER_ELECTION_NAMESPACE, else POD_NAMESPACE
	LeaderElectionID            string        // LEADER_ELECTION_ID (smb-mount-healer-controlplane)
	CSINodeDriverNamespace      string        // CSI_NODE_DRIVER_NAMESPACE (kube-system)
	CSINodeDriverLabelSelector  string        // CSI_NODE_DRIVER_LABEL_SELECTOR (app=csi-smb-node)
	PollInterval                time.Duration // POLL_INTERVAL (3s)
	PlanningProbeWait           time.Duration // PLANNING_PROBE_WAIT (15s)
	ScaleDownWait               time.Duration // SCALE_DOWN_WAIT (120s)
	UnstageWait                 time.Duration // UNSTAGE_WAIT (30s)
	QuarantineUnstageWait       time.Duration // QUARANTINE_UNSTAGE_WAIT (300s)
	DriverRestartWait           time.Duration // DRIVER_RESTART_WAIT (90s)
	VerifyWait                  time.Duration // VERIFY_WAIT (90s)
	VerifyNoPodGrace            time.Duration // VERIFY_NO_POD_GRACE (30s)
	EpisodeMaxDuration          time.Duration // EPISODE_MAX_DURATION (30m)
	BackoffBase                 time.Duration // BACKOFF_BASE (5m)
	BackoffMax                  time.Duration // BACKOFF_MAX (1h)
	RemediationRetention        time.Duration // REMEDIATION_RETENTION (24h)
	RemediationRetainMinPerNode int           // REMEDIATION_RETAIN_MIN_PER_NODE (10)
}

// Probe is the hidden --probe child configuration.
type Probe struct {
	Op   string
	Args []string
}

type flagValues struct {
	controlplane, worker, dryRun, version, leaderElect bool
	probe, metricsAddr, healthAddr                     string
}

const hiddenProbeFlag = "probe"

// bindFlags registers the healer's flags on fs. Callers may register further
// flags (e.g. controller-runtime zap options) on the same FlagSet before Parse.
func bindFlags(fs *flag.FlagSet, v *flagValues) {
	fs.BoolVar(&v.controlplane, "controlplane", false, "run the leader-elected control plane (exactly one of --controlplane/--worker)")
	fs.BoolVar(&v.worker, "worker", false, "run the privileged per-node worker (exactly one of --controlplane/--worker)")
	fs.BoolVar(&v.dryRun, "dry-run", false, "controlplane only: walk remediations without scaling, deleting pods or touching hosts (also DRY_RUN=true)")
	fs.BoolVar(&v.version, "version", false, "print the version and exit")
	fs.BoolVar(&v.leaderElect, "leader-elect", true, "controlplane only: enable leader election")
	fs.StringVar(&v.metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	fs.StringVar(&v.healthAddr, "health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	fs.StringVar(&v.probe, hiddenProbeFlag, "", "internal: run a single probe child operation")
}

// NewFlagSet returns a FlagSet with the healer's flags registered, for use with Parse.
func NewFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// Parse parses args (without the program name) and the environment.
// fs must come from NewFlagSet; extra flags may already be registered on it.
// Errors wrap ErrUsage or ErrInvalid; -h/--help returns flag.ErrHelp.
func Parse(fs *flag.FlagSet, args []string, getenv func(string) string) (Config, error) {
	var v flagValues
	bindFlags(fs, &v)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, err
		}
		return Config{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}

	if v.probe != "" {
		return Config{Mode: ModeProbe, Probe: Probe{Op: v.probe, Args: fs.Args()}}, nil
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("%w: unexpected arguments %q", ErrUsage, fs.Args())
	}
	if v.version {
		return Config{Mode: ModeVersion}, nil
	}
	if v.controlplane == v.worker {
		return Config{}, fmt.Errorf("%w: exactly one of --controlplane or --worker is required", ErrUsage)
	}
	if v.worker && v.dryRun {
		return Config{}, fmt.Errorf("%w: --dry-run is only valid with --controlplane", ErrUsage)
	}

	e := env{getenv: getenv}
	cfg := Config{Common: Common{
		MetricsBindAddress:     v.metricsAddr,
		HealthProbeBindAddress: v.healthAddr,
		CSIDriverName:          e.str("CSI_DRIVER_NAME", "smb.csi.k8s.io"),
	}}

	if v.worker {
		cfg.Mode = ModeWorker
		cfg.Worker = parseWorker(&e)
	} else {
		cfg.Mode = ModeControlPlane
		cfg.ControlPlane = parseControlPlane(&e, v)
	}
	if err := e.err(); err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseWorker(e *env) Worker {
	root := e.str("KUBELET_ROOT", "/var/lib/kubelet")
	return Worker{
		NodeName:                 e.str("NODE_NAME", ""),
		KubeletRoot:              root,
		ExpectedFSTypes:          strings.Fields(e.str("EXPECTED_FSTYPES", "cifs smb3")),
		CheckInterval:            e.dur("CHECK_INTERVAL", 60*time.Second),
		RecheckAttempts:          int32(e.integer("RECHECK_ATTEMPTS", 3, 1<<20)), //nolint:gosec // bounded by integer()
		RecheckDelay:             e.dur("RECHECK_DELAY", 3*time.Second),
		StatTimeout:              e.dur("STAT_TIMEOUT", 5*time.Second),
		StartupGrace:             e.dur("STARTUP_GRACE", 10*time.Second),
		PodStartupGrace:          e.dur("POD_STARTUP_GRACE", 60*time.Second),
		SlowConfirmWindow:        e.dur("SLOW_CONFIRM_WINDOW", 3*time.Minute),
		ProbeConcurrency:         int(e.integer("PROBE_CONCURRENCY", 4, 1024)),
		StatusHeartbeat:          e.dur("STATUS_HEARTBEAT", 5*time.Minute),
		QuarantineDir:            e.str("QUARANTINE_DIR", filepath.Join(root, "smb-mount-healer-quarantine")),
		QuarantineScanInterval:   e.dur("QUARANTINE_SCAN_INTERVAL", 5*time.Minute),
		QuarantineScanMaxEntries: e.integer("QUARANTINE_SCAN_MAX_ENTRIES", 100000, 1<<40),
		QuarantineScanTimeBudget: e.dur("QUARANTINE_SCAN_TIME_BUDGET", 10*time.Second),
	}
}

func parseControlPlane(e *env, v flagValues) ControlPlane {
	ns := e.str("LEADER_ELECTION_NAMESPACE", "")
	if ns == "" {
		ns = e.str("POD_NAMESPACE", "")
	}
	return ControlPlane{
		DryRun:                      v.dryRun || e.boolean("DRY_RUN", false),
		LeaderElection:              v.leaderElect,
		LeaderElectionNamespace:     ns,
		LeaderElectionID:            e.str("LEADER_ELECTION_ID", "smb-mount-healer-controlplane"),
		CSINodeDriverNamespace:      e.str("CSI_NODE_DRIVER_NAMESPACE", "kube-system"),
		CSINodeDriverLabelSelector:  e.str("CSI_NODE_DRIVER_LABEL_SELECTOR", "app=csi-smb-node"),
		PollInterval:                e.dur("POLL_INTERVAL", 3*time.Second),
		PlanningProbeWait:           e.dur("PLANNING_PROBE_WAIT", 15*time.Second),
		ScaleDownWait:               e.dur("SCALE_DOWN_WAIT", 120*time.Second),
		UnstageWait:                 e.dur("UNSTAGE_WAIT", 30*time.Second),
		QuarantineUnstageWait:       e.dur("QUARANTINE_UNSTAGE_WAIT", 300*time.Second),
		DriverRestartWait:           e.dur("DRIVER_RESTART_WAIT", 90*time.Second),
		VerifyWait:                  e.dur("VERIFY_WAIT", 90*time.Second),
		VerifyNoPodGrace:            e.dur("VERIFY_NO_POD_GRACE", 30*time.Second),
		EpisodeMaxDuration:          e.dur("EPISODE_MAX_DURATION", 30*time.Minute),
		BackoffBase:                 e.dur("BACKOFF_BASE", 5*time.Minute),
		BackoffMax:                  e.dur("BACKOFF_MAX", time.Hour),
		RemediationRetention:        e.dur("REMEDIATION_RETENTION", 24*time.Hour),
		RemediationRetainMinPerNode: int(e.integer("REMEDIATION_RETAIN_MIN_PER_NODE", 10, 1<<20)),
	}
}

// Validate checks cross-field constraints. Errors wrap ErrInvalid.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.Common.CSIDriverName == "" {
		add("CSI_DRIVER_NAME must not be empty")
	}
	switch c.Mode {
	case ModeWorker:
		w := c.Worker
		if w.NodeName == "" {
			add("NODE_NAME is required in worker mode")
		}
		if !filepath.IsAbs(w.KubeletRoot) {
			add("KUBELET_ROOT must be absolute, got %q", w.KubeletRoot)
		}
		if !filepath.IsAbs(w.QuarantineDir) {
			add("QUARANTINE_DIR must be absolute, got %q", w.QuarantineDir)
		}
		if len(w.ExpectedFSTypes) == 0 {
			add("EXPECTED_FSTYPES must not be empty")
		}
		positive(add, map[string]time.Duration{
			"CHECK_INTERVAL": w.CheckInterval, "RECHECK_DELAY": w.RecheckDelay, "STAT_TIMEOUT": w.StatTimeout,
			"SLOW_CONFIRM_WINDOW": w.SlowConfirmWindow, "STATUS_HEARTBEAT": w.StatusHeartbeat,
			"QUARANTINE_SCAN_INTERVAL": w.QuarantineScanInterval, "QUARANTINE_SCAN_TIME_BUDGET": w.QuarantineScanTimeBudget,
		})
		if w.StartupGrace < 0 || w.PodStartupGrace < 0 {
			add("STARTUP_GRACE and POD_STARTUP_GRACE must not be negative")
		}
		if w.RecheckAttempts < 1 {
			add("RECHECK_ATTEMPTS must be >= 1")
		}
		if w.ProbeConcurrency < 1 {
			add("PROBE_CONCURRENCY must be >= 1")
		}
		if w.QuarantineScanMaxEntries < 1 {
			add("QUARANTINE_SCAN_MAX_ENTRIES must be >= 1")
		}
		if w.StatTimeout >= w.CheckInterval {
			add("STAT_TIMEOUT (%s) must be shorter than CHECK_INTERVAL (%s)", w.StatTimeout, w.CheckInterval)
		}
	case ModeControlPlane:
		cp := c.ControlPlane
		if cp.CSINodeDriverNamespace == "" {
			add("CSI_NODE_DRIVER_NAMESPACE must not be empty")
		}
		if sel, err := labels.Parse(cp.CSINodeDriverLabelSelector); err != nil {
			add("CSI_NODE_DRIVER_LABEL_SELECTOR: %v", err)
		} else if sel.Empty() {
			add("CSI_NODE_DRIVER_LABEL_SELECTOR must not be empty")
		}
		if cp.LeaderElection && cp.LeaderElectionID == "" {
			add("LEADER_ELECTION_ID must not be empty")
		}
		positive(add, map[string]time.Duration{
			"POLL_INTERVAL": cp.PollInterval, "PLANNING_PROBE_WAIT": cp.PlanningProbeWait,
			"SCALE_DOWN_WAIT": cp.ScaleDownWait, "UNSTAGE_WAIT": cp.UnstageWait,
			"QUARANTINE_UNSTAGE_WAIT": cp.QuarantineUnstageWait, "DRIVER_RESTART_WAIT": cp.DriverRestartWait,
			"VERIFY_WAIT": cp.VerifyWait, "VERIFY_NO_POD_GRACE": cp.VerifyNoPodGrace,
			"EPISODE_MAX_DURATION": cp.EpisodeMaxDuration, "BACKOFF_BASE": cp.BackoffBase,
			"BACKOFF_MAX": cp.BackoffMax, "REMEDIATION_RETENTION": cp.RemediationRetention,
		})
		if cp.BackoffBase > cp.BackoffMax {
			add("BACKOFF_BASE (%s) must not exceed BACKOFF_MAX (%s)", cp.BackoffBase, cp.BackoffMax)
		}
		if cp.RemediationRetainMinPerNode < 0 {
			add("REMEDIATION_RETAIN_MIN_PER_NODE must not be negative")
		}
	case ModeProbe, ModeVersion:
	default:
		add("unknown mode %q", c.Mode)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return nil
}

func positive(add func(string, ...any), ds map[string]time.Duration) {
	keys := make([]string, 0, len(ds))
	for k := range ds {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if ds[k] <= 0 {
			add("%s must be positive, got %s", k, ds[k])
		}
	}
}

// Usage writes the usage text (without the hidden --probe flag) to w.
func Usage(w io.Writer, fs *flag.FlagSet) {
	_, _ = fmt.Fprintf(w, "Usage: %s (--controlplane [--dry-run] | --worker) [flags]\n       %s --version\n\nFlags:\n", fs.Name(), fs.Name())
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == hiddenProbeFlag {
			return
		}
		def := ""
		if f.DefValue != "" && f.DefValue != "false" {
			def = fmt.Sprintf(" (default %q)", f.DefValue)
		}
		_, _ = fmt.Fprintf(w, "  --%s\n\t%s%s\n", f.Name, f.Usage, def)
	})
	_, _ = fmt.Fprintln(w, "\nAll other settings come from environment variables; see README.md.")
}
