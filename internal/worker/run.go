package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/linux"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/internal/probe"
)

// probeRunnerCloseTimeout bounds how long shutdown waits for probe children.
const probeRunnerCloseTimeout = 5 * time.Second

// Options are dependencies injectable by tests and internal/sim.
type Options struct {
	Clock    clock.Clock // defaults to clock.RealClock{}
	Version  string
	Revision string
}

// Run starts the worker for cfg.Worker.NodeName on the real host
// (hostfs/linux + probe children) and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, opts Options) error {
	if opts.Clock == nil {
		opts.Clock = clock.RealClock{}
	}
	log := logr.FromContextOrDiscard(ctx)
	wc := cfg.Worker
	log.Info("starting worker", "node", wc.NodeName, "version", opts.Version, "revision", opts.Revision,
		"kubeletRoot", wc.KubeletRoot, "quarantineDir", wc.QuarantineDir)

	scheme, err := kube.NewScheme()
	if err != nil {
		return err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	mopts, err := ManagerOptions(cfg, scheme)
	if err != nil {
		return err
	}
	mopts.Logger = log
	mgr, err := ctrl.NewManager(restCfg, mopts)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}

	m := metrics.NewWorker(wc.NodeName)
	if err := m.Register(ctrlmetrics.Registry); err != nil {
		return fmt.Errorf("register metrics: %w", err)
	}
	if err := metrics.RegisterBuildInfo(ctrlmetrics.Registry, metrics.ComponentWorker, opts.Version, opts.Revision); err != nil {
		return fmt.Errorf("register build_info: %w", err)
	}

	runner := probe.NewRunner(probe.Options{Timeout: wc.StatTimeout, ExpectedFSTypes: wc.ExpectedFSTypes})
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeRunnerCloseTimeout)
		defer cancel()
		if err := runner.Close(cctx); err != nil {
			log.Error(err, "probe children still alive at shutdown", "hung", runner.Hung())
		}
	}()
	if _, err := New(mgr, Deps{
		Config:        wc,
		CSIDriverName: cfg.Common.CSIDriverName,
		Host:          LinuxHost(wc, cfg.Common.CSIDriverName, runner),
		Clock:         opts.Clock,
		Metrics:       m,
		Recorder:      mgr.GetEventRecorder(EventRecorderName),
	}); err != nil {
		return err
	}
	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// LinuxHost returns the real host layer: mountinfo filtered on the kubelet
// root, the kubelet dir inspector, runner as prober and unmounter, and the
// bounded quarantine inspector.
func LinuxHost(wc config.Worker, driver string, runner *probe.Runner) Host {
	mounts := linux.NewMountTable(wc.KubeletRoot)
	return Host{
		Mounts:  mounts,
		Dirs:    linux.DirInspector{Layout: kubelet.Layout{Root: wc.KubeletRoot, DriverName: driver}, Mounts: mounts},
		Prober:  runner,
		Mutator: &linux.Mutator{Root: wc.KubeletRoot, Mounts: mounts, Unmounter: runner},
		Quarantine: linux.QuarantineInspector{
			Dir:        wc.QuarantineDir,
			MaxEntries: wc.QuarantineScanMaxEntries,
			TimeBudget: wc.QuarantineScanTimeBudget,
			Mounts:     linux.NewMountTable(wc.QuarantineDir),
		},
	}
}
