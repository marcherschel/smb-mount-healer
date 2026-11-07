package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

var hostExecNow = time.Date(2026, 10, 11, 7, 8, 9, 0, time.UTC)

type hostExecFixture struct {
	host    *fake.Host
	exec    *HostExecutor
	layout  kubelet.Layout
	probed  []string
	scanned []string
	gm, pod string
}

func newHostExecFixture(t *testing.T) *hostExecFixture {
	t.Helper()
	layout := kubelet.Layout{Root: "/var/lib/kubelet", DriverName: "smb.csi.k8s.io"}
	f := &hostExecFixture{layout: layout}
	f.host = fake.NewHost(layout, "/var/lib/kubelet/smb-mount-healer-quarantine", "cifs", "smb3")
	f.gm = layout.GlobalmountPath("storagebox-transmission-config")
	f.pod = layout.PodVolumeMountPath("u-1", "storagebox-transmission-config")
	f.exec = &HostExecutor{
		KubeletRoot:   layout.Root,
		QuarantineDir: f.host.Quarantine.Dir,
		Mounts:        f.host.Mounts,
		Dirs:          f.host.Dirs,
		Prober:        f.host.Prober,
		Mutator:       f.host.Mutator,
		Clock:         clocktesting.NewFakePassiveClock(hostExecNow),
		ProbeNow:      func(p string) { f.probed = append(f.probed, p) },
		OnQuarantined: func(d string) { f.scanned = append(f.scanned, d) },
	}
	return f
}

func (f *hostExecFixture) run(t *testing.T, dryRun bool, cmd command.HostCommand) v1alpha1.HostActionResult {
	t.Helper()
	res := f.exec.ExecuteHost(context.Background(), dryRun, cmd)
	if res.ID != cmd.ID() || !res.Time.Time.Equal(hostExecNow) {
		t.Fatalf("result id/time: %+v", res)
	}
	return res
}

func TestHostExecutorForceUnmount(t *testing.T) {
	f := newHostExecFixture(t)
	f.host.Mounts.Mount(f.pod, "cifs", "//srv", "soft")
	cmd := command.ForceUnmount{ActionID: "fu-1", Path: f.pod, OnlyIf: v1alpha1.OnlyIfNotHealthyMount}

	// Healthy cifs: skipped.
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionSkipped {
		t.Fatalf("healthy: %+v", res)
	}
	// Probe infrastructure error: never act on a guess.
	f.host.Prober.Script(f.pod, hostfs.ProbeError)
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionFailed || !f.host.Mounts.IsMountpoint(f.pod) {
		t.Fatalf("probe error: %+v", res)
	}
	// xfs overmount (2026-08-29 shape): not healthy -> dry-run only logs.
	f.host.Mounts.Mount(f.pod, "xfs", "/dev/vda6", "")
	if res := f.run(t, true, cmd); res.Result != v1alpha1.HostActionDryRun || len(f.host.Mutator.Calls()) != 0 {
		t.Fatalf("dry-run: %+v %v", res, f.host.Mutator.Calls())
	}
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionSucceeded {
		t.Fatalf("live: %+v", res)
	}
	// The healthy cifs mount below is visible again: re-running is a skip.
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionSkipped {
		t.Fatalf("rerun: %+v", res)
	}
	// A hung stat counts as not healthy.
	f.host.Prober.SetHung(f.pod, true)
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionSucceeded || f.host.Mounts.IsMountpoint(f.pod) {
		t.Fatalf("hung: %+v", res)
	}
	// Nothing mounted any more: idempotent success, no mutation.
	calls := len(f.host.Mutator.Calls())
	if res := f.run(t, false, command.ForceUnmount{ActionID: "fu-2", Path: f.pod}); res.Result != v1alpha1.HostActionSucceeded {
		t.Fatalf("unmounted: %+v", res)
	}
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionSucceeded || len(f.host.Mutator.Calls()) != calls+2 {
		t.Fatalf("not a mountpoint: %+v", res)
	}
	f.host.Mutator.SetError(f.gm, errors.New("umount: device or resource busy"))
	f.host.Mounts.Mount(f.gm, "cifs", "//srv", "")
	if res := f.run(t, false, command.ForceUnmount{ActionID: "fu-3", Path: f.gm}); res.Result != v1alpha1.HostActionFailed || !strings.Contains(res.Error, "busy") {
		t.Fatalf("mutator error: %+v", res)
	}
}

func TestHostExecutorQuarantine(t *testing.T) {
	f := newHostExecFixture(t)
	cmd := command.Quarantine{ActionID: "q-1", Path: f.gm, PV: "storagebox-transmission-config", OnlyIf: v1alpha1.OnlyIfNotMountpoint}

	// Live mount: skipped by onlyIf, nothing moved.
	f.host.Mounts.Mount(f.gm, "cifs", "//srv", "soft")
	f.host.Dirs.SetDir(f.gm, fake.Dir{Entries: 3, Files: 5, Bytes: 100})
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionSkipped {
		t.Fatalf("mounted: %+v", res)
	}
	// Without onlyIf the mutator-level refusal still protects the mount.
	noIf := cmd
	noIf.OnlyIf = ""
	if res := f.run(t, false, noIf); res.Result != v1alpha1.HostActionFailed || !strings.Contains(res.Error, "mountpoint") {
		t.Fatalf("mounted, no onlyIf: %+v", res)
	}
	f.host.Mounts.Unmount(f.gm)

	if res := f.run(t, true, cmd); res.Result != v1alpha1.HostActionDryRun || res.Destination != "" {
		t.Fatalf("dry-run: %+v", res)
	}
	if d, _ := f.host.Dirs.Dir(f.gm); d.Entries != 3 {
		t.Fatal("dry-run moved data")
	}

	res := f.run(t, false, cmd)
	want := "/var/lib/kubelet/smb-mount-healer-quarantine/storagebox-transmission-config-20261011T070809Z"
	if res.Result != v1alpha1.HostActionSucceeded || res.Destination != want || res.Entries != 3 {
		t.Fatalf("live: %+v", res)
	}
	if len(f.scanned) != 1 || f.scanned[0] != want {
		t.Fatalf("OnQuarantined: %v", f.scanned)
	}
	// Re-run (worker restarted before recording the result): empty, nothing new.
	if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionSucceeded || res.Destination != "" || res.Entries != 0 {
		t.Fatalf("rerun: %+v", res)
	}
	// Second shadow in the same second: collision suffix.
	f.host.Dirs.SetDir(f.gm, fake.Dir{Entries: 1})
	if res := f.run(t, false, cmd); res.Destination != want+"-2" {
		t.Fatalf("collision: %+v", res)
	}
	// Missing dir: nothing to do.
	missing := command.Quarantine{ActionID: "q-2", Path: f.pod, PV: "pv"}
	if res := f.run(t, false, missing); res.Result != v1alpha1.HostActionSucceeded || res.Destination != "" {
		t.Fatalf("missing: %+v", res)
	}
	// Dry run with nothing to quarantine still reports DryRun, not
	// Succeeded (e2e S16: dry-run tasks must not claim real results).
	if res := f.run(t, true, missing); res.Result != v1alpha1.HostActionDryRun || res.Destination != "" {
		t.Fatalf("dry-run missing: %+v", res)
	}
	f.host.Dirs.SetDir(f.gm, fake.Dir{})
	if res := f.run(t, true, cmd); res.Result != v1alpha1.HostActionDryRun || res.Destination != "" {
		t.Fatalf("dry-run empty: %+v", res)
	}
}

func TestHostExecutorRejectsBadInput(t *testing.T) {
	f := newHostExecFixture(t)
	for _, cmd := range []command.HostCommand{
		command.ForceUnmount{ActionID: "a", Path: "/etc"},
		command.ForceUnmount{ActionID: "b", Path: "/var/lib/kubelet"},
		command.ForceUnmount{ActionID: "c", Path: "/var/lib/kubelet/../../etc"},
		command.Quarantine{ActionID: "d", Path: f.gm, PV: "../x"},
		command.Quarantine{ActionID: "e", Path: "/var/lib/kubelet-other/x", PV: "pv"},
		command.ProbeNow{ActionID: "f", Path: "relative"},
	} {
		if res := f.run(t, false, cmd); res.Result != v1alpha1.HostActionFailed {
			t.Fatalf("%s: %+v", cmd.ID(), res)
		}
	}
	if len(f.host.Mutator.Calls()) != 0 {
		t.Fatalf("mutated on bad input: %+v", f.host.Mutator.Calls())
	}
}

func TestHostExecutorProbeNow(t *testing.T) {
	f := newHostExecFixture(t)
	if res := f.run(t, false, command.ProbeNow{ActionID: "p1", Path: f.pod}); res.Result != v1alpha1.HostActionSucceeded {
		t.Fatalf("%+v", res)
	}
	if res := f.run(t, true, command.ProbeNow{ActionID: "p2"}); res.Result != v1alpha1.HostActionDryRun {
		t.Fatalf("%+v", res)
	}
	if len(f.probed) != 2 || f.probed[0] != f.pod || f.probed[1] != "" {
		t.Fatalf("probed = %q", f.probed)
	}
	f.exec.ProbeNow = nil
	if res := f.run(t, false, command.ProbeNow{ActionID: "p3"}); res.Result != v1alpha1.HostActionFailed {
		t.Fatalf("%+v", res)
	}
}
