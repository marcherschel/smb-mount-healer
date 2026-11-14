package sim

import (
	"fmt"
	"strings"
	"time"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// Fixtures mirror test/e2e: one samba server with shares config-a, config-b,
// config-c, dl and library; samba-landmine for the dead-orphan case.
const (
	fxDeploySingle = "deploy-single"
	fxStsSingle    = "sts-single"
	fxTwoVol       = "two-vol"
	fxCotenants    = "cotenants"
	fxLandmine     = "landmine"
	fxSpare        = "spare"
)

func fixtures(w *World, which ...string) {
	for _, f := range which {
		switch f {
		case fxDeploySingle:
			w.AddPV("config-a", SambaServer, "config-a")
			w.AddDeployment("deploy-single", 1, nil, "config-a")
		case fxStsSingle:
			w.AddPV("config-b", SambaServer, "config-b")
			w.AddStatefulSet("sts-single", 1, nil, "config-b")
		case fxTwoVol:
			w.AddPV("config-c", SambaServer, "config-c")
			w.AddPV("dl", SambaServer, "dl")
			w.AddDeployment("two-vol", 1, nil, "config-c", "dl")
		case fxCotenants:
			w.AddPV("library", SambaServer, "library")
			w.AddDeployment("cotenant-a", 1, nil, "library")
			w.AddDeployment("cotenant-b", 1, nil, "library")
		case fxLandmine:
			w.AddPV("landmine", SambaLandmine, "landmine")
		case fxSpare:
			w.AddPV("spare", SambaServer, "spare")
		}
	}
}

// metricValue sums a metric's series whose labels include want.
func (w *World) metricValue(name string, want map[string]string) float64 {
	fams, err := w.reg.Gather()
	if err != nil {
		return -1
	}
	sum := 0.0
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			have := map[string]string{}
			for _, l := range m.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			for k, v := range want {
				if have[k] != v {
					continue series
				}
			}
			if c := m.GetCounter(); c != nil {
				sum += c.GetValue()
			} else if g := m.GetGauge(); g != nil {
				sum += g.GetValue()
			}
		}
	}
	return sum
}

func atLeast(w *World, name string, labels map[string]string, n float64) []string {
	if v := w.metricValue(name, labels); v < n {
		return []string{fmt.Sprintf("%s%v = %v, want >= %v", name, labels, v, n)}
	}
	return nil
}

func exactly(w *World, name string, labels map[string]string, n float64) []string {
	if v := w.metricValue(name, labels); v != n {
		return []string{fmt.Sprintf("%s%v = %v, want %v", name, labels, v, n)}
	}
	return nil
}

func traceHas(w *World, substr string) []string {
	if !strings.Contains(string(w.trace.Bytes()), substr) {
		return []string{"trace does not contain " + substr}
	}
	return nil
}

// injectWrongFSType is the incident-1 injection (e2e S4): the globalmount
// is detached behind kubelet's back and the pod is recreated, so the new pod
// gets the bare globalmount dir bind-mounted (xfs).
func injectWrongFSType(w *World, pv, workload string) {
	w.After(10*time.Second, "umount -l gm("+pv+"); delete a pod of "+workload, func() {
		w.DetachGlobalmount("n1", pv)
		w.Workload(workload).DeletePod()
	})
}

func s4Setup(w *World) { fixtures(w, fxDeploySingle) }

func s4Script(w *World) { injectWrongFSType(w, "config-a", "deploy-single") }

func checkHealedS4(w *World) []string {
	var p []string
	p = append(p, atLeast(w, metrics.WorkloadActionsTotal, map[string]string{"action": "scale_down", "mode": "live"}, 1)...)
	p = append(p, atLeast(w, metrics.WorkloadActionsTotal, map[string]string{"action": "scale_up", "mode": "live"}, 1)...)
	p = append(p, atLeast(w, metrics.WorkloadActionsTotal, map[string]string{"action": "driver_restart", "mode": "live"}, 1)...)
	p = append(p, atLeast(w, metrics.HostActionsTotal, map[string]string{"action": "quarantine", "result": "succeeded"}, 1)...)
	if !w.Samba.HasFile(SambaServer, "config-a", "hb-deploy-single-") {
		p = append(p, "no heartbeat in samba share config-a")
	}
	return p
}

// sweepS6Script is e2e S6: detach the globalmount, write a file into the
// bare directory and scale the app to 0; afterwards scale it back.
func sweepS6Script(w *World) {
	w.After(10*time.Second, "umount -l gm(config-a); write a file into the bare dir; scale deploy-single to 0", func() {
		w.DetachGlobalmount("n1", "config-a")
		w.WriteGlobalmountDir("n1", "config-a", "shadow.txt")
		w.Workload("deploy-single").SetReplicas(0, "human", true)
	})
	w.When("sweep finished: scale deploy-single back to 1", func() bool { return len(w.Outcomes()) > 0 && w.ActiveRemediation() == "" },
		func() { w.Workload("deploy-single").SetReplicas(1, "human", true) })
}

func checkSweepS6(w *World) []string {
	var p []string
	qs := w.Quarantines()
	if len(qs) != 1 {
		return append(p, fmt.Sprintf("want 1 SmbQuarantine, have %d", len(qs)))
	}
	q := qs[0]
	if q.Spec.PVName != "config-a" || q.Status.Files != 1 || q.Spec.RemediationRef == "" {
		p = append(p, fmt.Sprintf("SmbQuarantine %s: pv=%s files=%d remediationRef=%q", q.Name, q.Spec.PVName, q.Status.Files, q.Spec.RemediationRef))
	}
	return append(p, exactly(w, metrics.HostActionsTotal, map[string]string{"action": "quarantine", "result": "succeeded"}, 1)...)
}

// Scenarios returns every scenario (mirroring e2e S1-S17 plus crash,
// leader-change and multi-node cases), in a stable order.
func Scenarios() []Scenario {
	ss := []Scenario{
		{
			Name: "s01-baseline", Max: 5 * time.Minute,
			Setup: func(w *World) { fixtures(w, fxDeploySingle, fxStsSingle, fxTwoVol, fxCotenants) },
			Check: func(w *World) []string {
				var p []string
				for _, sm := range w.smbMounts() {
					if g := sm.Status.Globalmount; g == nil || !g.Mounted || !strings.Contains(g.SuperOptions, "soft") {
						p = append(p, "SmbMount "+sm.Name+": globalmount not mounted with soft")
					}
				}
				return append(p, exactly(w, metrics.RemediationsStartedTotal, nil, 0)...)
			},
		},
		{
			Name: "s02-transient-blip", Max: 5 * time.Minute, Opts: shortCheckInterval(),
			Setup: s4Setup,
			Script: func(w *World) {
				w.After(10*time.Second, "pkill -STOP smbd (7s)", func() { w.Samba.Set(SambaServer, ServerPaused) })
				w.After(17*time.Second, "pkill -CONT smbd", func() { w.Samba.Set(SambaServer, ServerUp) })
			},
			Check: func(w *World) []string {
				return append(atLeast(w, metrics.FalseAlarmsTotal, nil, 1), traceHas(w, "Suspect")...)
			},
		},
		{
			Name: "s03-slow-server", Max: 6 * time.Minute, Opts: shortCheckInterval(),
			Setup: s4Setup,
			Script: func(w *World) {
				w.After(10*time.Second, "pkill -STOP smbd (20s, shorter than SLOW_CONFIRM_WINDOW)", func() { w.Samba.Set(SambaServer, ServerPaused) })
				w.After(30*time.Second, "pkill -CONT smbd", func() { w.Samba.Set(SambaServer, ServerUp) })
			},
			Check: func(w *World) []string {
				p := atLeast(w, metrics.MountChecksTotal, map[string]string{"result": string(hostfs.ProbeHung)}, 1)
				p = append(p, atLeast(w, metrics.MountChecksTotal, map[string]string{"result": string(hostfs.ProbeStatTimeout)}, 1)...)
				return append(p, exactly(w, metrics.ProbesHung, nil, 0)...)
			},
		},
		s03bHungServer(),
		s03cFlappingServer(),
		s03dFlappingServerNoRecoveryWindow(),
		{
			Name: "s04-wrong-fstype", Max: 20 * time.Minute, Setup: s4Setup, Script: s4Script,
			Expect: []string{"Workload:healed"}, Check: checkHealedS4,
		},
		{
			// A StatefulSet pod is only recreated once the old one is gone,
			// by which time kubelet has unstaged the empty, detached
			// globalmount: the plain S4 injection heals itself (s05b). It
			// stays poisoned when shadow data blocks the unstage: the new
			// pod cancels the pending unstage and gets the bare dir.
			Name: "s05-statefulset", Max: 20 * time.Minute,
			Setup: func(w *World) { fixtures(w, fxStsSingle) },
			Script: func(w *World) {
				w.After(10*time.Second, "umount -l gm(config-b); write a file into the bare dir; delete sts-single-0", func() {
					w.DetachGlobalmount("n1", "config-b")
					w.WriteGlobalmountDir("n1", "config-b", "leftover.txt")
					w.Workload("sts-single").DeletePod()
				})
			},
			// The worker publishes the one tick without a pod: a Sweep is
			// created, finds nothing after its probe (nothing_to_sweep), and
			// still backs the PV off, delaying the Workload remediation by
			// BACKOFF_BASE (see TestBugNoOpSweepBacksOff).
			Expect: []string{"Sweep:healed", "Workload:healed"},
			Check: func(w *World) []string {
				p := atLeast(w, metrics.WorkloadActionsTotal, map[string]string{"action": "driver_restart"}, 1)
				if !w.Samba.HasFile(SambaServer, "config-b", "hb-sts-single-0") {
					p = append(p, "no heartbeat in samba share config-b")
				}
				return p
			},
		},
		{
			Name: "s05b-statefulset-self-heals", Max: 4 * time.Minute,
			Setup:  func(w *World) { fixtures(w, fxStsSingle) },
			Script: func(w *World) { injectWrongFSType(w, "config-b", "sts-single") },
			Check: func(w *World) []string {
				return append(exactly(w, metrics.RemediationsStartedTotal, nil, 0), traceHas(w, "unstage config-b: ok")...)
			},
		},
		{
			Name: "s06-shadow-globalmount", Max: 20 * time.Minute, Setup: s4Setup, Script: sweepS6Script,
			Expect: []string{"Sweep:healed"}, Check: checkSweepS6,
		},
		{
			Name: "s07-orphan-pod-dir", Max: 20 * time.Minute,
			Setup: func(w *World) { fixtures(w, fxDeploySingle, fxSpare) },
			Script: func(w *World) {
				w.After(10*time.Second, "seed pods/<rand-uid>/volumes/kubernetes.io~csi/spare/mount/old.txt", func() {
					w.SeedOrphanPodDir("n1", "0b5e0d0c-dead-4bad-8bad-0123456789ab", "spare", "old.txt")
				})
			},
			Expect: []string{"Sweep:healed"},
			Check: func(w *World) []string {
				qs := w.Quarantines()
				if len(qs) != 1 || qs[0].Spec.PVName != "spare" || qs[0].Spec.RemediationRef == "" {
					return []string{fmt.Sprintf("want one SmbQuarantine of spare with a remediationRef, have %d", len(qs))}
				}
				return atLeast(w, metrics.HostActionsTotal, map[string]string{"action": "quarantine", "result": "succeeded"}, 1)
			},
		},
		{
			Name: "s08-dead-orphan-globalmount", Max: 20 * time.Minute,
			Setup: func(w *World) { fixtures(w, fxDeploySingle, fxLandmine) },
			Script: func(w *World) {
				w.After(time.Second, "cifs-mount samba-landmine onto gm(landmine) and onto an unknown globalmount hash", func() {
					w.MountForeign("n1", w.PVByName("landmine").GMPath, SambaLandmine, "landmine")
					w.MountForeign("n1", w.UnknownGlobalmountPath(), SambaLandmine, "landmine")
				})
				w.After(90*time.Second, "delete samba-landmine", func() { w.Samba.Set(SambaLandmine, ServerDown) })
			},
			Expect: []string{"Sweep:healed"},
			Check: func(w *World) []string {
				p := atLeast(w, metrics.HostActionsTotal, map[string]string{"action": "force_unmount", "result": "succeeded"}, 1)
				if !w.Node("n1").Host.Mounts.IsMountpoint(w.UnknownGlobalmountPath()) {
					p = append(p, "the globalmount under an unknown hash was touched")
				}
				return p
			},
		},
		{
			Name: "s09-cotenant-pinning", Max: 20 * time.Minute,
			Setup:  func(w *World) { fixtures(w, fxCotenants) },
			Script: func(w *World) { injectWrongFSType(w, "library", "cotenant-b") },
			Expect: []string{"Workload:healed"},
			Check: func(w *World) []string {
				p := exactly(w, metrics.WorkloadActionsTotal, map[string]string{"action": "scale_down"}, 2)
				return append(p, exactly(w, metrics.WorkloadVerificationsTotal, map[string]string{"result": "verified"}, 2)...)
			},
		},
		{
			Name: "s10-flux-fighter", Max: 20 * time.Minute,
			Setup: func(w *World) { fixtures(w, fxCotenants) },
			Script: func(w *World) {
				injectWrongFSType(w, "library", "cotenant-b")
				f := w.AddFighter(w.Workload("cotenant-a"), 1, 15*time.Second)
				w.OnStage(v1alpha1.StageDraining, "flux-fighter starts rescaling cotenant-a to 1 (first in 2s, then every 15s)",
					func() { f.Start(2 * time.Second) })
				w.OnStage(v1alpha1.StageScalingUp, "flux-fighter stops", func() { f.Active = false })
			},
			Expect: []string{"Workload:healed"},
			Check: func(w *World) []string {
				return atLeast(w, metrics.WorkloadActionsTotal, map[string]string{"action": "scale_reassert"}, 1)
			},
		},
		{
			Name: "s11-two-volume-pod", Max: 20 * time.Minute,
			Setup:  func(w *World) { fixtures(w, fxTwoVol) },
			Script: func(w *World) { injectWrongFSType(w, "dl", "two-vol") },
			Expect: []string{"Workload:healed"},
			Check:  func(w *World) []string { return traceHas(w, "pvs=[config-c dl]") },
		},
		{
			Name: "s12-controlplane-crash", Max: 25 * time.Minute, Setup: s4Setup,
			Script: func(w *World) {
				s4Script(w)
				w.OnStage(v1alpha1.StageAwaitingUnstageCompletion, "kill the control plane for 60s", func() { w.cp.Crash(time.Minute) })
			},
			Expect: []string{"Workload:healed"},
			Check: func(w *World) []string {
				return append(checkHealedS4(w), atLeast(w, metrics.RemediationsResumedTotal, nil, 1)...)
			},
		},
		{
			Name: "s12a-worker-crash-mid-task", Max: 25 * time.Minute, Setup: s4Setup,
			Script: func(w *World) {
				s4Script(w)
				w.CrashWorkerMidTask("n1", "quarantining", 0)
			},
			Expect: []string{"Workload:healed"},
			Check: func(w *World) []string {
				p := checkHealedS4(w)
				if w.workerCrash == nil || !w.workerCrash.fired {
					p = append(p, "worker crash point never fired")
				}
				return append(p, exactly(w, metrics.RemediationsStartedTotal, nil, 1)...)
			},
		},
		{
			Name: "s12b-remediation-deleted", Max: 25 * time.Minute, Setup: s4Setup,
			Script: func(w *World) {
				s4Script(w)
				w.OnStage(v1alpha1.StageDraining, "kubectl delete smbr (during draining)", w.DeleteRemediation)
			},
			Expect: []string{"Workload:rolled_back", "Workload:healed"},
			Check: func(w *World) []string {
				return atLeast(w, metrics.WorkloadActionsTotal, map[string]string{"action": "rollback"}, 1)
			},
		},
		{
			Name: "s12c-remediation-aborted", Max: 30 * time.Minute, Setup: s4Setup,
			Script: func(w *World) {
				s4Script(w)
				w.OnStage(v1alpha1.StageDraining, "kubectl annotate smbr abort=true (during draining)", w.AbortRemediation)
			},
			Expect: []string{"Workload:rolled_back", "Workload:healed"},
			Check: func(w *World) []string {
				return atLeast(w, metrics.RemediationSuppressedTotal, map[string]string{"reason": "backoff"}, 1)
			},
		},
		{
			Name: "s13-samba-new-ip", Max: 20 * time.Minute, Setup: s4Setup,
			Script: s13Script,
			Expect: []string{"Workload:healed"},
			Check: func(w *World) []string {
				return exactly(w, metrics.RemediationsStartedTotal, nil, 1)
			},
		},
		{
			Name: "s14-outage-during-episode", Max: 40 * time.Minute, Setup: s4Setup,
			Script: func(w *World) {
				s4Script(w)
				w.OnStage(v1alpha1.StageDraining, "samba scaled to 0 for 4m", func() {
					w.Samba.Set(SambaServer, ServerDown)
					w.After(4*time.Minute, "samba back", func() { w.Samba.Set(SambaServer, ServerUp) })
				})
			},
			Expect: []string{"Workload:verify_failed"},
			Check: func(w *World) []string {
				return atLeast(w, metrics.RemediationSuppressedTotal, map[string]string{"reason": "backoff"}, 1)
			},
		},
		{
			Name: "s15-unowned-cotenant", Max: 25 * time.Minute, Unhealed: true, Settle: 30 * time.Second,
			Setup: func(w *World) {
				fixtures(w, fxCotenants)
				w.AddBarePod("bare-cotenant", "n1", "library")
			},
			Script: func(w *World) { injectWrongFSType(w, "library", "cotenant-b") },
			Expect: []string{"Workload:verify_failed"},
			Check: func(w *World) []string {
				return append(traceHas(w, "UnstagePinned"), atLeast(w, metrics.RemediationsTotal, map[string]string{"outcome": "verify_failed"}, 1)...)
			},
		},
		{
			Name: "s16-dry-run", Max: 20 * time.Minute, Unhealed: true, Settle: 30 * time.Second,
			Opts: Options{DryRun: true}, Setup: s4Setup, Script: s4Script,
			Expect: []string{"Workload:dry_run"},
			Check: func(w *World) []string {
				p := exactly(w, metrics.WorkloadActionsTotal, map[string]string{"mode": "live"}, 0)
				if qs := w.Quarantines(); len(qs) != 0 {
					p = append(p, "dry run created an SmbQuarantine")
				}
				return append(p, atLeast(w, metrics.HostActionsTotal, map[string]string{"dry_run": "true"}, 1)...)
			},
		},
		s17QuarantineLifecycle(),
		{
			Name: "x01-two-node-scope-contention", Max: 40 * time.Minute,
			Opts: Options{Nodes: []string{"n1", "n2"}},
			Setup: func(w *World) {
				w.AddPV("library", SambaServer, "library")
				w.AddDeployment("spread", 2, []string{"n1", "n2"}, "library")
			},
			Script: func(w *World) {
				w.After(10*time.Second, "umount -l gm(library) on n1 and n2; delete both pods of spread", func() {
					w.DetachGlobalmount("n1", "library")
					w.DetachGlobalmount("n2", "library")
					wl := w.Workload("spread")
					wl.DeletePod()
					wl.DeletePod()
				})
			},
			// Both nodes trigger in the same round. n2's remediation waits in
			// pending until n1 scales the shared Deployment down, then finds
			// its broken pod terminating and ends skipped_no_owner, which
			// backs n2 off for BACKOFF_BASE (see TestBugSpuriousSkippedNoOwner).
			Expect: []string{"Workload:healed", "Workload:healed"},
		},
		{
			Name: "x02-leader-change", Max: 20 * time.Minute, Setup: s4Setup,
			Script: func(w *World) {
				if err := w.cp.PrepareStandby(); err != nil {
					w.violation("standby: %v", err)
				}
				s4Script(w)
				w.OnStage(v1alpha1.StageQuarantining, "leader election moves to the standby replica", w.cp.LeaderChange)
			},
			Expect: []string{"Workload:healed"},
			Check: func(w *World) []string {
				return append(checkHealedS4(w), atLeast(w, metrics.RemediationsResumedTotal, nil, 1)...)
			},
		},
	}
	ss = append(ss, crashScenarios()...)
	return ss
}

// shortCheckInterval mirrors the e2e short-timeout overlay for the probe
// cadence scenarios (a 7s blip is invisible at CHECK_INTERVAL=60s).
func shortCheckInterval() Options {
	return Options{Worker: func(c *config.Worker) { c.CheckInterval = 5 * time.Second }}
}

// s03bHungServer: smbd stopped for longer than SLOW_CONFIRM_WINDOW. Probes
// hang, the drain times out because kubelet's unmount of the pod path hangs
// too, releasing_mounts force-unmounts it, awaiting_unstage force-unmounts
// the dead globalmount; smbd continues while the driver restarts.
func s03bHungServer() Scenario {
	return Scenario{
		Name: "s03b-hung-server", Max: 25 * time.Minute, Setup: s4Setup, Script: hungServerScript,
		Expect: []string{"Workload:healed"},
		Check: func(w *World) []string {
			p := traceHas(w, "DrainTimeout")
			p = append(p, atLeast(w, metrics.HostActionsTotal, map[string]string{"action": "force_unmount", "result": "succeeded"}, 2)...)
			return append(p, exactly(w, metrics.ProbesHung, nil, 0)...)
		},
	}
}

// s03cFlappingServer is the mount that keeps flapping after e2e S3 netem:
// slow failures with ok probes in between. SLOW_RECOVERY_WINDOW keeps the
// streak across the ok probes (Suspect, no false alarms), so it is Broken
// once its first failure is SLOW_CONFIRM_WINDOW old, and the remediation's
// remount (driver restart) clears it.
func s03cFlappingServer() Scenario {
	return Scenario{
		Name: "s03c-flapping-server", Max: 25 * time.Minute, Opts: shortCheckInterval(), Setup: s4Setup,
		Script: func(w *World) {
			w.After(10*time.Second, "cifs client degraded: slow 10s of every 30s", func() { w.Samba.Set(SambaServer, ServerFlapping) })
			w.OnStage(v1alpha1.StageRestartingDriver, "remount clears the degraded client", func() { w.Samba.Set(SambaServer, ServerUp) })
		},
		Expect: []string{"Workload:healed"},
		Check: func(w *World) []string {
			p := exactly(w, metrics.FalseAlarmsTotal, nil, 0)
			return append(p, atLeast(w, metrics.MountChecksTotal, map[string]string{"result": string(hostfs.ProbeOK)}, 1)...)
		},
	}
}

// s03dFlappingServerNoRecoveryWindow is s03c with SLOW_RECOVERY_WINDOW=0
// (the behaviour before it existed): every ok probe ends the streak with a
// false alarm, the mount is never confirmed and nothing remediates it.
func s03dFlappingServerNoRecoveryWindow() Scenario {
	return Scenario{
		Name: "s03d-flapping-server-no-recovery-window", Max: 10 * time.Minute, Unhealed: true, Settle: time.Second,
		Opts: Options{Worker: func(c *config.Worker) {
			c.CheckInterval = 5 * time.Second
			c.SlowRecoveryWindow = 0
		}},
		Setup: s4Setup,
		Script: func(w *World) {
			w.After(10*time.Second, "cifs client degraded: slow 10s of every 30s", func() { w.Samba.Set(SambaServer, ServerFlapping) })
		},
		Check: func(w *World) []string { return atLeast(w, metrics.FalseAlarmsTotal, nil, 10) },
	}
}

func hungServerScript(w *World) {
	w.After(10*time.Second, "pkill -STOP smbd", func() { w.Samba.Set(SambaServer, ServerPaused) })
	w.OnStage(v1alpha1.StageRestartingDriver, "pkill -CONT smbd", func() { w.Samba.Set(SambaServer, ServerUp) })
}

func s13Script(w *World) {
	w.After(10*time.Second, "delete the samba pod (comes back with a new IP)", func() { w.Samba.NewIP(SambaServer) })
}

func s17QuarantineLifecycle() Scenario {
	folder := func(w *World) string {
		for _, q := range w.Quarantines() {
			if q.Spec.RemediationRef != "" {
				return q.Spec.Path[strings.LastIndex(q.Spec.Path, "/")+1:]
			}
		}
		return ""
	}
	return Scenario{
		Name: "s17-quarantine-lifecycle", Max: 20 * time.Minute, Settle: 25 * time.Minute, Setup: s4Setup,
		Script: func(w *World) {
			sweepS6Script(w)
			fx := w.WriteGlobalmountDir // keep the S6 fixture symmetric: two files quarantined
			w.After(11*time.Second, "write a second file into the bare dir", func() { fx("n1", "config-a", "shadow-2.txt") })
			w.When("quarantined", func() bool { return folder(w) != "" }, func() {
				name := folder(w)
				q := w.Node("n1").Host.Quarantine
				w.After(time.Minute, "(a) remove half of the files from the quarantine folder", func() {
					st, err := q.Scan(w.ctx, QuarantineDir+"/"+name)
					w.Expectf(err == nil && st.Files == 2, "quarantine folder holds 2 files: %+v %v", st, err)
					q.SetFolder(name, hostfs.QuarantineStats{Entries: 1, Files: 1, Bytes: st.Bytes / 2,
						OldestModTime: st.OldestModTime, NewestModTime: st.NewestModTime})
				})
				w.After(7*time.Minute, "(b) kubectl delete smbq", func() {
					qs := w.Quarantines()
					w.Expectf(len(qs) == 1 && qs[0].Status.Files == 1, "files decreased to 1 after a scan: %d objects", len(qs))
					if len(qs) == 1 {
						w.DeleteQuarantineObject(qs[0].Name)
					}
				})
				w.After(8*time.Minute, "(c) move the folder out of QUARANTINE_DIR", func() {
					w.Expectf(len(w.Quarantines()) == 1, "smbq recreated while data remains")
					q.RemoveFolder(name)
				})
				w.After(14*time.Minute, "(d) seed a bash-style <pv>-<ts> folder", func() {
					w.Expectf(len(w.Quarantines()) == 0, "smbq deleted within one scan interval of the folder disappearing")
					q.SetFolder("config-a-20260101T000000Z", hostfs.QuarantineStats{Entries: 3, Files: 3, Bytes: 300})
				})
			})
		},
		Expect: []string{"Sweep:healed"},
		Check: func(w *World) []string {
			qs := w.Quarantines()
			if len(qs) != 1 || qs[0].Spec.RemediationRef != "" || qs[0].Spec.PVName != "config-a" || qs[0].Status.Files != 3 {
				return []string{fmt.Sprintf("bash-style folder not adopted: %d objects", len(qs))}
			}
			return nil
		},
	}
}

// crashScenarios kill the control plane at every stage the episode
// persists, both between reconciles and right after a write-ahead status
// write (before that Step's commands ran), and the worker at stages with
// host work; each must resume to the same outcome.
func crashScenarios() []Scenario {
	var out []Scenario
	// Each stage is crashed in a scenario where it persists across a
	// write: releasing_mounts needs a drain-pod mount left (hung server),
	// awaiting_unstage a dead globalmount (samba new IP).
	workload := []struct {
		st     v1alpha1.Stage
		script func(*World)
	}{
		{v1alpha1.StagePending, s4Script}, {v1alpha1.StagePlanning, s4Script}, {v1alpha1.StageDraining, s4Script},
		{v1alpha1.StageReleasingMounts, hungServerScript}, {v1alpha1.StageAwaitingUnstage, s13Script},
		{v1alpha1.StageQuarantining, s4Script}, {v1alpha1.StageAwaitingUnstageCompletion, s4Script},
		{v1alpha1.StageRestartingDriver, s4Script}, {v1alpha1.StageScalingUp, s4Script},
		{v1alpha1.StageVerifying, s4Script}, {v1alpha1.StageDone, s4Script},
	}
	for _, c := range workload {
		out = append(out, cpCrash("workload", c.st, s4Setup, c.script, "Workload:healed"))
	}
	sweep := []v1alpha1.Stage{v1alpha1.StagePlanning, v1alpha1.StageSweeping, v1alpha1.StageAwaitingUnstageCompletion, v1alpha1.StageDone}
	for _, st := range sweep {
		out = append(out, cpCrash("sweep", st, s4Setup, sweepS6Script, "Sweep:healed"))
	}
	for _, c := range []struct {
		st     v1alpha1.Stage
		script func(*World)
	}{{v1alpha1.StagePlanning, s4Script}, {v1alpha1.StageReleasingMounts, hungServerScript}, {v1alpha1.StageVerifying, s4Script}} {
		out = append(out, Scenario{
			Name: "w-restart-" + strings.ReplaceAll(string(c.st), "_", "-"), Max: 25 * time.Minute, Setup: s4Setup,
			Script: func(w *World) {
				c.script(w)
				w.OnStage(c.st, "restart the worker", func() { w.Node("n1").CrashWorker(0) })
			},
			Expect: []string{"Workload:healed"},
			Check: func(w *World) []string {
				return exactly(w, metrics.RemediationsStartedTotal, nil, 1)
			},
		})
	}
	return out
}

func cpCrash(machine string, st v1alpha1.Stage, setup, script func(*World), expect string) Scenario {
	stage := strings.ReplaceAll(string(st), "_", "-")
	return Scenario{
		Name: "cp-crash-" + machine + "-" + stage, Max: 30 * time.Minute, Setup: setup,
		Script: func(w *World) {
			script(w)
			w.CrashControlPlaneAfterWrite(st, 0)
		},
		Expect: []string{expect},
		Check: func(w *World) []string {
			if w.cpCrash == nil || !w.cpCrash.fired {
				return []string{"write-ahead crash point for stage " + string(st) + " never fired"}
			}
			return nil
		},
	}
}
