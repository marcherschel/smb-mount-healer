//go:build e2e

package framework

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// Kill deletes the healer pod of component (the control plane, or the worker
// on the agent node) and waits until a replacement with a new UID is ready.
func (f *F) Kill(component string) *corev1.Pod {
	f.T.Helper()
	node := ""
	if component == metrics.ComponentWorker {
		node = f.AgentNode
	}
	sel := ComponentLabel + "=" + component
	old := f.ReadyPod(HealerNamespace, sel, node)
	f.G.Expect(f.Kube.CoreV1().Pods(HealerNamespace).Delete(f.Ctx, old.Name, metav1.DeleteOptions{})).To(gomega.Succeed())
	var p *corev1.Pod
	f.G.Eventually(func(g gomega.Gomega) {
		var err error
		p, err = f.readyPod(HealerNamespace, sel, node)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(p.UID).NotTo(gomega.Equal(old.UID))
	}).WithTimeout(3 * time.Minute).Should(gomega.Succeed())
	return p
}

// BaselineTimeout is DefaultBaselineTimeout or E2E_BASELINE_TIMEOUT (Go duration).
func BaselineTimeout() time.Duration {
	if v := os.Getenv("E2E_BASELINE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return DefaultBaselineTimeout
}

// WaitBaseline waits until the cluster is back at the steady state every test
// starts from (registered by New as a t.Cleanup):
//   - samba and samba-landmine ready, flux-fighter disabled;
//   - every fixture workload at its manifest replicas, all ready;
//   - an SmbMount for every staged fixture PV on the agent node, and every
//     SmbMount Healthy;
//   - no non-terminal SmbMountRemediation;
//   - no SmbQuarantine, unless the test opted in with AllowQuarantines;
//   - the worker quiet: no stat_timeout/probe_hung check for BaselineQuietWindow.
//
// The first call of a test then resets the remediation history (resetHistory),
// so remediations that finished while it waited do not count as the test's.
//
// The last point matters on newer kernels (6.17 on the hosted runner): after
// a slow-server fault a CIFS connection can stay wedged for minutes, so its
// mounts flap Suspect/Healthy and every prober round runs into STAT_TIMEOUT.
// A 3s poll can land on a Healthy moment and let the next test start against
// a degraded worker (slow probe rounds), which the 6.8 client never shows.
func (f *F) WaitBaseline() {
	f.T.Helper()
	start := time.Now()
	f.G.Eventually(func(g gomega.Gomega) {
		g.Expect(f.baselineProblems()).To(gomega.BeEmpty())
		g.Expect(f.workerQuiet()).To(gomega.Succeed())
	}).WithTimeout(BaselineTimeout()).WithPolling(3*time.Second).Should(gomega.Succeed(),
		"cluster did not return to baseline")
	if d := time.Since(start); d > 2*BaselineQuietWindow {
		f.Logf("baseline reached after %s", d.Round(time.Second))
	}
	// Tests call WaitBaseline first thing, before they inject anything; later
	// calls (mid-test, cleanup) keep the test's own remediations.
	if !f.baselined {
		f.baselined = true
		f.resetHistory()
	}
}

// BaselineQuietWindow is how long the worker must go without a slow probe
// (stat_timeout/probe_hung) for WaitBaseline: longer than CHECK_INTERVAL of
// values-e2e.yaml (10s); a wedged mount fails every ~6s (RECHECK_DELAY +
// STAT_TIMEOUT).
const BaselineQuietWindow = 15 * time.Second

// quietUntil: within 10s of a confirmed quiet window it is not repeated (the
// cleanup WaitBaseline of one test and the start of the next).
var quietUntil time.Time

// workerQuiet scrapes the agent worker twice, BaselineQuietWindow apart, and
// fails if a slow probe was counted in between (or the worker restarted).
func (f *F) workerQuiet() error {
	if time.Now().Before(quietUntil) {
		return nil
	}
	slow := func() (float64, error) {
		s, err := f.scrape(metrics.ComponentWorker)
		if err != nil {
			return 0, err
		}
		return s.Value(metrics.MountChecksTotal, Labels{metrics.LabelResult: string(v1alpha1.ReasonStatTimeout)}) +
			s.Value(metrics.MountChecksTotal, Labels{metrics.LabelResult: string(v1alpha1.ReasonProbeHung)}), nil
	}
	before, err := slow()
	if err != nil {
		return err
	}
	select {
	case <-f.Ctx.Done():
		return f.Ctx.Err()
	case <-time.After(BaselineQuietWindow):
	}
	after, err := slow()
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("worker not quiet: slow probe checks %v -> %v within %s", before, after, BaselineQuietWindow)
	}
	quietUntil = time.Now().Add(10 * time.Second)
	return nil
}

// baselineProblems lists every deviation from baseline (empty = baseline).
func (f *F) baselineProblems() []string {
	var probs []string
	add := func(format string, args ...any) { probs = append(probs, fmt.Sprintf(format, args...)) }

	for _, s := range []*Samba{f.Samba, f.Landmine} {
		if _, err := f.readyPod(SambaNamespace, s.selector(), ""); err != nil {
			add("samba %s: %v", s.Name, err)
		}
	}
	if on, err := f.Fixtures.FighterEnabled(); err != nil || on {
		add("flux-fighter enabled=%v err=%v", on, err)
	}
	for _, w := range Workloads {
		spec, ready, err := f.Fixtures.Replicas(w.Name)
		switch {
		case err != nil:
			add("workload %s: %v", w.Name, err)
		case spec != w.Replicas || ready != w.Replicas:
			add("workload %s: replicas spec=%d ready=%d, want %d", w.Name, spec, ready, w.Replicas)
		}
	}

	var mounts v1alpha1.SmbMountList
	if err := f.Client.List(f.Ctx, &mounts); err != nil {
		add("list smbm: %v", err)
	} else {
		have := map[string]bool{}
		for _, m := range mounts.Items {
			have[m.Name] = true
			if m.Status.Health != v1alpha1.HealthHealthy {
				add("smbm %s: health=%q reason=%q", m.Name, m.Status.Health, m.Status.Reason)
			}
		}
		for _, pv := range StagedPVs() {
			if n := v1alpha1.SmbMountName(f.AgentNode, pv); !have[n] {
				add("smbm %s missing", n)
			}
		}
	}

	var rems v1alpha1.SmbMountRemediationList
	if err := f.Client.List(f.Ctx, &rems); err != nil {
		add("list smbr: %v", err)
	} else {
		for _, r := range rems.Items {
			if !r.Status.Stage.Terminal() {
				add("smbr %s non-terminal: stage=%q", r.Name, r.Status.Stage)
			}
		}
	}

	if !f.allowQuarantines {
		var qs v1alpha1.SmbQuarantineList
		if err := f.Client.List(f.Ctx, &qs); err != nil {
			add("list smbq: %v", err)
		} else if len(qs.Items) > 0 {
			names := make([]string, 0, len(qs.Items))
			for _, q := range qs.Items {
				names = append(names, q.Name)
			}
			add("unexpected smbq: %s", strings.Join(names, ", "))
		}
	}
	return probs
}
