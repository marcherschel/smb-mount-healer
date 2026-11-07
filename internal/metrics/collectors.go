package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Bucket layouts.
var (
	checkDurationBuckets       = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	stageDurationBuckets       = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600}
	remediationDurationBuckets = []float64{30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 7200}
)

func counterVec(name, help string, cl prometheus.Labels, labels []string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help, ConstLabels: cl}, labels)
}

func gaugeVec(name, help string, cl prometheus.Labels, labels []string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help, ConstLabels: cl}, labels)
}

func histogramVec(name, help string, cl prometheus.Labels, buckets []float64, labels []string) *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, ConstLabels: cl, Buckets: buckets}, labels)
}

func registerAll(r prometheus.Registerer, cs ...prometheus.Collector) error {
	var errs []error
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Worker holds the worker's collectors. All carry const labels component="worker" and node.
type Worker struct {
	MountChecks               *prometheus.CounterVec
	MountCheckDuration        *prometheus.HistogramVec
	MountStatus               *prometheus.GaugeVec
	HealthTransitions         *prometheus.CounterVec
	FalseAlarms               *prometheus.CounterVec
	ProbesHung                prometheus.Gauge
	UnknownGlobalmounts       prometheus.Gauge
	Quarantines               *prometheus.GaugeVec
	QuarantineBytes           *prometheus.GaugeVec
	QuarantineOldestTimestamp *prometheus.GaugeVec
	QuarantineScans           *prometheus.CounterVec
	HostActions               *prometheus.CounterVec
	SmbMountStatusWrites      *prometheus.CounterVec
}

// NewWorker creates (but does not register) the worker collectors for node.
func NewWorker(node string) *Worker {
	cl := prometheus.Labels{LabelComponent: ComponentWorker, LabelNode: node}
	return &Worker{
		MountChecks:        counterVec(MountChecksTotal, "Mount probes run, by mount kind and probe class.", cl, MountChecksTotalLabels),
		MountCheckDuration: histogramVec(MountCheckDurationSeconds, "Duration of mount probes.", cl, checkDurationBuckets, MountCheckDurationSecondsLabels),
		MountStatus:        gaugeVec(MountStatus, "Debounced health of each mount (enum gauge: 1 for the current status).", cl, MountStatusLabels),
		HealthTransitions:  counterVec(HealthTransitionsTotal, "Health state transitions of mounts.", cl, HealthTransitionsTotalLabels),
		FalseAlarms:        counterVec(FalseAlarmsTotal, "Mounts that went Suspect and recovered without remediation.", cl, FalseAlarmsTotalLabels),
		ProbesHung: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: ProbesHung, Help: "Probe children currently stuck (e.g. D-state).", ConstLabels: cl,
		}),
		UnknownGlobalmounts: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: UnknownGlobalmounts, Help: "Globalmount dirs whose hash matches no known smb PV.", ConstLabels: cl,
		}),
		Quarantines:               gaugeVec(Quarantines, "Pending quarantine folders (SmbQuarantine objects) per PV.", cl, QuarantinesLabels),
		QuarantineBytes:           gaugeVec(QuarantineBytes, "Bytes held in quarantine folders per PV.", cl, QuarantineBytesLabels),
		QuarantineOldestTimestamp: gaugeVec(QuarantineOldestTimestampSeconds, "Unix time of the oldest quarantine folder per PV.", cl, QuarantineOldestTimestampSecondsLabels),
		QuarantineScans:           counterVec(QuarantineScansTotal, "Quarantine directory scans.", cl, QuarantineScansTotalLabels),
		HostActions:               counterVec(HostActionsTotal, "Host actions executed for SmbHostTasks.", cl, HostActionsTotalLabels),
		SmbMountStatusWrites:      counterVec(SmbMountStatusWritesTotal, "SmbMount status server-side applies.", cl, SmbMountStatusWritesTotalLabels),
	}
}

// Register registers every worker collector on r.
func (w *Worker) Register(r prometheus.Registerer) error {
	return registerAll(r, w.MountChecks, w.MountCheckDuration, w.MountStatus, w.HealthTransitions, w.FalseAlarms,
		w.ProbesHung, w.UnknownGlobalmounts, w.Quarantines, w.QuarantineBytes, w.QuarantineOldestTimestamp,
		w.QuarantineScans, w.HostActions, w.SmbMountStatusWrites)
}

// ControlPlane holds the control plane's collectors. All carry const label component="controlplane".
type ControlPlane struct {
	RemediationStage         *prometheus.GaugeVec
	RemediationTransitions   *prometheus.CounterVec
	RemediationStageDuration *prometheus.HistogramVec
	RemediationsStarted      *prometheus.CounterVec
	Remediations             *prometheus.CounterVec
	RemediationDuration      *prometheus.HistogramVec
	RemediationsResumed      *prometheus.CounterVec
	RemediationSuppressed    *prometheus.CounterVec
	WorkloadActions          *prometheus.CounterVec
	WorkloadVerifications    *prometheus.CounterVec
	SmbMounts                *prometheus.GaugeVec
	DryRun                   prometheus.Gauge
}

// NewControlPlane creates (but does not register) the control-plane collectors.
func NewControlPlane() *ControlPlane {
	cl := prometheus.Labels{LabelComponent: ComponentControlPlane}
	return &ControlPlane{
		RemediationStage:         gaugeVec(RemediationStage, "Current stage of active remediations (enum gauge).", cl, RemediationStageLabels),
		RemediationTransitions:   counterVec(RemediationTransitionsTotal, "Remediation stage transitions.", cl, RemediationTransitionsTotalLabels),
		RemediationStageDuration: histogramVec(RemediationStageDurationSeconds, "Time spent in each remediation stage.", cl, stageDurationBuckets, RemediationStageDurationSecondsLabels),
		RemediationsStarted:      counterVec(RemediationsStartedTotal, "Remediations created.", cl, RemediationsStartedTotalLabels),
		Remediations:             counterVec(RemediationsTotal, "Remediations finished, by outcome.", cl, RemediationsTotalLabels),
		RemediationDuration:      histogramVec(RemediationDurationSeconds, "Duration of finished remediations.", cl, remediationDurationBuckets, RemediationDurationSecondsLabels),
		RemediationsResumed:      counterVec(RemediationsResumedTotal, "Non-terminal remediations resumed after a restart or leader change.", cl, RemediationsResumedTotalLabels),
		RemediationSuppressed:    counterVec(RemediationSuppressedTotal, "Remediation triggers suppressed.", cl, RemediationSuppressedTotalLabels),
		WorkloadActions:          counterVec(WorkloadActionsTotal, "Workload actions (scale_down, scale_reassert, scale_up, driver_restart, rollback).", cl, WorkloadActionsTotalLabels),
		WorkloadVerifications:    counterVec(WorkloadVerificationsTotal, "Post-remediation workload verifications.", cl, WorkloadVerificationsTotalLabels),
		SmbMounts:                gaugeVec(SmbMounts, "SmbMount objects across the cluster, by health.", cl, SmbMountsLabels),
		DryRun: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: DryRun, Help: "1 if the control plane runs with --dry-run (remediations only simulate), 0 if it enforces.", ConstLabels: cl,
		}),
	}
}

// Register registers every control-plane collector on r.
func (c *ControlPlane) Register(r prometheus.Registerer) error {
	return registerAll(r, c.RemediationStage, c.RemediationTransitions, c.RemediationStageDuration,
		c.RemediationsStarted, c.Remediations, c.RemediationDuration, c.RemediationsResumed,
		c.RemediationSuppressed, c.WorkloadActions, c.WorkloadVerifications, c.SmbMounts, c.DryRun)
}

// SetDryRun exports the control plane's mode on the dry_run gauge.
func (c *ControlPlane) SetDryRun(dryRun bool) {
	v := 0.0
	if dryRun {
		v = 1
	}
	c.DryRun.Set(v)
}

// RegisterBuildInfo registers build_info{component,version,revision,mode} = 1.
// mode and component are ComponentWorker or ComponentControlPlane.
func RegisterBuildInfo(r prometheus.Registerer, mode, version, revision string) error {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: BuildInfo, Help: "Build information; always 1.", ConstLabels: prometheus.Labels{LabelComponent: mode},
	}, BuildInfoLabels)
	g.WithLabelValues(version, revision, mode).Set(1)
	return r.Register(g)
}
