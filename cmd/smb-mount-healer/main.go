// Command smb-mount-healer detects and heals broken SMB (CIFS) CSI mounts.
//
// Exactly one mode per process:
//
//	smb-mount-healer --controlplane [--dry-run]   leader-elected decision maker (Deployment)
//	smb-mount-healer --worker                     privileged per-node prober/executor (DaemonSet)
//	smb-mount-healer --version
//
// A hidden "--probe <op> <path>" mode is the self-exec child used by the worker.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/controlplane"
	"github.com/marcherschel/smb-mount-healer/internal/probe"
	"github.com/marcherschel/smb-mount-healer/internal/worker"
)

// Set via -ldflags "-X main.version=... -X main.revision=...".
var (
	version  = "dev"
	revision = "unknown"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := config.NewFlagSet("smb-mount-healer")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(fs)

	cfg, err := config.Parse(fs, args, os.Getenv)
	switch {
	case errors.Is(err, flag.ErrHelp):
		config.Usage(os.Stdout, fs)
		return exitOK
	case errors.Is(err, config.ErrUsage):
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		config.Usage(os.Stderr, fs)
		return exitUsage
	case err != nil:
		_, _ = fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitUsage
	}

	switch cfg.Mode {
	case config.ModeProbe:
		// Must stay first and minimal: no logging, no clients.
		return probe.RunChild(cfg.Probe.Op, cfg.Probe.Args)
	case config.ModeVersion:
		_, _ = fmt.Printf("smb-mount-healer %s (%s, %s)\n", version, revision, runtime.Version())
		return exitOK
	case config.ModeControlPlane, config.ModeWorker:
	}

	logger := zap.New(zap.UseFlagOptions(&zapOpts)).WithValues("mode", string(cfg.Mode))
	ctrl.SetLogger(logger)
	ctx := ctrl.LoggerInto(ctrl.SetupSignalHandler(), logger)

	if err := runMode(ctx, cfg); err != nil {
		logger.Error(err, "exiting")
		return exitRuntime
	}
	return exitOK
}

func runMode(ctx context.Context, cfg config.Config) error {
	switch cfg.Mode {
	case config.ModeControlPlane:
		return controlplane.Run(ctx, cfg, controlplane.Options{Version: version, Revision: revision})
	case config.ModeWorker:
		return worker.Run(ctx, cfg, worker.Options{Version: version, Revision: revision})
	case config.ModeProbe, config.ModeVersion:
	}
	return fmt.Errorf("unexpected mode %q", cfg.Mode)
}
