package remediation

import (
	"testing"
	"time"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// TestVerifyReplacementWithoutSmbMount: during a share outage the episode
// unstaged the volume, the worker deleted the node's SmbMount, and the
// scaled-up replacement pod (still on the node, observed through its PVC)
// cannot mount. It is neither "verified" nor "elsewhere": verification
// stays pending until VERIFY_WAIT and the outcome is verify_failed (e2e S14).
func TestVerifyReplacementWithoutSmbMount(t *testing.T) {
	w, obj, cfg := at(t, v1alpha1.RemediationTypeWorkload, v1alpha1.StageVerifying,
		func(w *world) { w.newHealth = v1alpha1.HealthUnknown })
	delete(w.mounts, "pv-a")
	var fresh int
	for _, p := range w.pods {
		if p.Owner != nil && !p.Terminating && len(p.PVs) > 0 {
			fresh++
		}
	}
	if fresh == 0 {
		t.Fatalf("no replacement pod observed: %+v", w.pods)
	}

	once(t, w, obj, cfg, cfg.VerifyNoPodGrace+time.Second, 1)
	if obj.Status.Stage != v1alpha1.StageVerifying {
		t.Fatalf("left verifying before VERIFY_WAIT: %s", trace(obj))
	}
	for _, v := range obj.Status.Verification {
		if v.Result != v1alpha1.VerificationPending {
			t.Fatalf("%s: %s, want pending (the pod is on the node)", v.Workload, v.Result)
		}
	}
	for i := 0; i < 10 && !obj.Status.Stage.Terminal(); i++ {
		once(t, w, obj, cfg, cfg.VerifyWait, 1)
	}
	if obj.Status.Outcome != v1alpha1.OutcomeVerifyFailed {
		t.Fatalf("outcome %q, want verify_failed; %s", obj.Status.Outcome, trace(obj))
	}
}
