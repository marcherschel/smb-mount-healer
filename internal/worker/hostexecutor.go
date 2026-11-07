package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// HostExecutor implements command.HostExecutor over the hostfs interfaces.
//
// Every action re-checks its OnlyIf against a fresh mountinfo snapshot (and,
// for NotHealthyMount, a fresh probe) right before acting, so running the
// same action twice is safe: a second ForceUnmount finds nothing mounted, a
// second Quarantine finds the directory empty. Dry-run evaluates the
// precondition, logs "would ..." and returns HostActionDryRun without
// touching anything. Error texts carry only healer-owned paths and errnos,
// never entry names from user data.
type HostExecutor struct {
	// KubeletRoot: every action path must lie strictly below it.
	KubeletRoot   string
	QuarantineDir string
	Mounts        hostfs.MountTable
	Dirs          hostfs.DirInspector
	Prober        hostfs.Prober
	Mutator       hostfs.Mutator
	Clock         clock.PassiveClock // defaults to clock.RealClock{}
	// ProbeNow asks the ProberRunnable for an immediate probe of path ("" =
	// every mount on the node). It must not block. Nil makes ProbeNow fail.
	ProbeNow func(path string)
	// OnQuarantined, if set, is called after a Quarantine created a folder
	// (the QuarantineScanner rescans right away).
	OnQuarantined func(destination string)
	// PlannedDestination, if set, is the quarantine folder the worker
	// recorded (write-ahead) in the task status before running the action.
	PlannedDestination string
}

var _ command.HostExecutor = (*HostExecutor)(nil)

// ExecuteHost implements command.HostExecutor.
func (e *HostExecutor) ExecuteHost(ctx context.Context, dryRun bool, cmd command.HostCommand) v1alpha1.HostActionResult {
	log := logr.FromContextOrDiscard(ctx).WithValues("action", cmd.ID(), "dryRun", dryRun)
	var res v1alpha1.HostActionResult
	switch c := cmd.(type) {
	case command.ForceUnmount:
		log = log.WithValues("type", v1alpha1.HostActionForceUnmount, "path", c.Path)
		res = e.forceUnmount(ctx, log, dryRun, c)
	case command.Quarantine:
		log = log.WithValues("type", v1alpha1.HostActionQuarantine, "path", c.Path, "pv", c.PV)
		res = e.quarantine(ctx, log, dryRun, c)
	case command.ProbeNow:
		log = log.WithValues("type", v1alpha1.HostActionProbeNow, "path", c.Path)
		res = e.probeNow(log, dryRun, c)
	default:
		res = failed(fmt.Errorf("unsupported host command %T", cmd))
	}
	res.ID = cmd.ID()
	res.Time = e.now()
	if res.Result == v1alpha1.HostActionFailed {
		log.Info("host action failed", "error", res.Error)
	} else {
		log.Info("host action done", "result", res.Result, "destination", res.Destination, "entries", res.Entries)
	}
	return res
}

func (e *HostExecutor) now() metav1.Time {
	if e.Clock == nil {
		return metav1.Now()
	}
	return metav1.NewTime(e.Clock.Now())
}

func failed(err error) v1alpha1.HostActionResult {
	msg := err.Error()
	const maxLen = 1024
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	return v1alpha1.HostActionResult{Result: v1alpha1.HostActionFailed, Error: msg}
}

func (e *HostExecutor) checkPath(path string) error {
	clean := filepath.Clean(path)
	root := filepath.Clean(e.KubeletRoot)
	if path == "" || clean != path || !filepath.IsAbs(path) || len(clean) <= len(root)+1 ||
		clean[:len(root)+1] != root+"/" {
		return fmt.Errorf("path %q is not a clean path strictly below %s", path, root)
	}
	return nil
}

// onlyIf re-evaluates the precondition. ok=false means "skip"; reason
// explains the decision for the log.
func (e *HostExecutor) onlyIf(ctx context.Context, cond v1alpha1.OnlyIf, path string) (ok bool, reason string, err error) {
	if cond == "" {
		return true, "no precondition", nil
	}
	snap, err := e.Mounts.Snapshot(ctx)
	if err != nil {
		return false, "", fmt.Errorf("onlyIf %s: %w", cond, err)
	}
	switch cond {
	case v1alpha1.OnlyIfNotMountpoint:
		if snap.IsMountpoint(path) {
			return false, "path is a mountpoint", nil
		}
		return true, "path is not a mountpoint", nil
	case v1alpha1.OnlyIfNotHealthyMount:
		if !snap.IsMountpoint(path) {
			return true, "path is not a mountpoint", nil
		}
		r := e.Prober.Probe(ctx, path, snap)
		switch r.Class {
		case hostfs.ProbeOK:
			return false, "mount probes healthy", nil
		case hostfs.ProbeError:
			// Cannot tell healthy from broken: never act on a guess.
			return false, "", fmt.Errorf("onlyIf %s: probe error: %s", cond, r.Detail)
		case hostfs.ProbeNotMountpoint, hostfs.ProbeWrongFSType, hostfs.ProbeStatError,
			hostfs.ProbeStatTimeout, hostfs.ProbeHung:
			return true, fmt.Sprintf("mount not healthy: %s %s", r.Class, r.Detail), nil
		}
		return false, "", fmt.Errorf("onlyIf %s: unknown probe class %q", cond, r.Class)
	}
	return false, "", fmt.Errorf("unknown onlyIf %q", cond)
}

func (e *HostExecutor) forceUnmount(ctx context.Context, log logr.Logger, dryRun bool, c command.ForceUnmount) v1alpha1.HostActionResult {
	if err := e.checkPath(c.Path); err != nil {
		return failed(err)
	}
	ok, reason, err := e.onlyIf(ctx, c.OnlyIf, c.Path)
	if err != nil {
		return failed(err)
	}
	if !ok {
		log.Info("skipping force-unmount", "onlyIf", c.OnlyIf, "reason", reason)
		return v1alpha1.HostActionResult{Result: v1alpha1.HostActionSkipped}
	}
	if dryRun {
		log.Info("dry-run: would force-unmount (umount -f -l)", "reason", reason)
		return v1alpha1.HostActionResult{Result: v1alpha1.HostActionDryRun}
	}
	log.Info("force-unmounting", "reason", reason)
	if err := e.Mutator.ForceUnmount(ctx, c.Path); err != nil {
		return failed(err)
	}
	return v1alpha1.HostActionResult{Result: v1alpha1.HostActionSucceeded}
}

func (e *HostExecutor) quarantine(ctx context.Context, log logr.Logger, dryRun bool, c command.Quarantine) v1alpha1.HostActionResult {
	if err := e.checkPath(c.Path); err != nil {
		return failed(err)
	}
	if c.PV == "" || filepath.Base(c.PV) != c.PV {
		return failed(fmt.Errorf("invalid pv %q", c.PV))
	}
	ok, reason, err := e.onlyIf(ctx, c.OnlyIf, c.Path)
	if err != nil {
		return failed(err)
	}
	if !ok {
		log.Info("skipping quarantine", "onlyIf", c.OnlyIf, "reason", reason)
		return v1alpha1.HostActionResult{Result: v1alpha1.HostActionSkipped}
	}
	facts, err := e.Dirs.Inspect(ctx, c.Path)
	if err != nil {
		// ErrIsMountpoint included: Quarantine never touches a live mount.
		return failed(err)
	}
	nothing := ""
	switch {
	case !facts.Exists:
		nothing = "path does not exist"
	case !facts.IsDir:
		return failed(fmt.Errorf("quarantine %s: not a directory", c.Path))
	case facts.Empty:
		nothing = "directory is empty"
	}
	if nothing != "" {
		if dryRun {
			// A dry-run task reports DryRun for every action it evaluated,
			// never a Succeeded that suggests something was done.
			log.Info("dry-run: nothing to quarantine: " + nothing)
			return v1alpha1.HostActionResult{Result: v1alpha1.HostActionDryRun}
		}
		log.Info("nothing to quarantine: " + nothing)
		return v1alpha1.HostActionResult{Result: v1alpha1.HostActionSucceeded}
	}
	dest := e.PlannedDestination
	if dest == "" {
		dest = filepath.Join(e.QuarantineDir, kubelet.QuarantineFolderName(c.PV, e.now().Time))
	}
	if dryRun {
		log.Info("dry-run: would quarantine local shadow data", "destination", dest)
		return v1alpha1.HostActionResult{Result: v1alpha1.HostActionDryRun}
	}
	mv, err := e.Mutator.Quarantine(ctx, c.Path, dest)
	res := v1alpha1.HostActionResult{
		Result:      v1alpha1.HostActionSucceeded,
		Destination: mv.Destination,
		Entries:     int32(min(mv.Entries, math.MaxInt32)), //nolint:gosec // clamped
	}
	if err != nil {
		f := failed(err)
		res.Result, res.Error = f.Result, f.Error
	}
	if mv.Destination != "" {
		log.Info("quarantined local shadow data; recover it manually, it is never deleted",
			"destination", mv.Destination, "entries", mv.Entries)
		if e.OnQuarantined != nil {
			e.OnQuarantined(mv.Destination)
		}
	}
	return res
}

func (e *HostExecutor) probeNow(log logr.Logger, dryRun bool, c command.ProbeNow) v1alpha1.HostActionResult {
	if c.Path != "" {
		if err := e.checkPath(c.Path); err != nil {
			return failed(err)
		}
	}
	if e.ProbeNow == nil {
		return failed(errors.New("probe trigger not configured"))
	}
	// A probe is read-only, so it runs in dry-run too (the control plane's
	// planning stage waits for fresh health either way).
	e.ProbeNow(c.Path)
	if dryRun {
		log.Info("dry-run: probe triggered (read-only)")
		return v1alpha1.HostActionResult{Result: v1alpha1.HostActionDryRun}
	}
	return v1alpha1.HostActionResult{Result: v1alpha1.HostActionSucceeded}
}
