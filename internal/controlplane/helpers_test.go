package controlplane

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func rem(name, node string, stage v1alpha1.Stage, finished time.Time) v1alpha1.SmbMountRemediation {
	ft := metav1.NewTime(finished)
	return v1alpha1.SmbMountRemediation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.SmbMountRemediationSpec{NodeName: node, Trigger: v1alpha1.RemediationTrigger{SmbMounts: []string{node + ".pv"}}},
		Status:     v1alpha1.SmbMountRemediationStatus{Stage: stage, StageEnteredTime: &ft, StartedTime: &ft},
	}
}

func TestSelectGC(t *testing.T) {
	var rems []v1alpha1.SmbMountRemediation
	for i := 0; i < 5; i++ {
		rems = append(rems, rem("r"+string(rune('a'+i)), "n", v1alpha1.StageDone, t0.Add(-time.Duration(i)*10*time.Hour)))
	}
	rems = append(rems, rem("active", "n", v1alpha1.StageDraining, t0.Add(-100*time.Hour)))
	del, next := selectGC(rems, t0, 24*time.Hour, 2)
	// ages 0,10,20,30,40h; keep newest 2; 30h and 40h expired; 20h expires in 4h.
	if len(del) != 2 || del[0].Name != "rd" || del[1].Name != "re" {
		t.Fatalf("del = %v", names(del))
	}
	if next != 4*time.Hour {
		t.Fatalf("next = %s", next)
	}
	if del, _ := selectGC(rems, t0, 24*time.Hour, 10); len(del) != 0 {
		t.Fatalf("keepMin must protect everything, got %v", names(del))
	}
}

func names(rs []v1alpha1.SmbMountRemediation) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestRemediationsLastHour(t *testing.T) {
	sm := &v1alpha1.SmbMount{ObjectMeta: metav1.ObjectMeta{Name: "n.pv"}, Spec: v1alpha1.SmbMountSpec{NodeName: "n", PVName: "pv"}}
	rems := []v1alpha1.SmbMountRemediation{
		rem("a", "n", v1alpha1.StageDone, t0.Add(-10*time.Minute)),
		rem("b", "n", v1alpha1.StageDraining, t0.Add(-30*time.Minute)),
		rem("c", "n", v1alpha1.StageDone, t0.Add(-2*time.Hour)),
		rem("d", "other", v1alpha1.StageDone, t0.Add(-time.Minute)),
	}
	n, exp := remediationsLastHour(rems, sm, t0)
	if n != 2 || !exp.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("n=%d exp=%s", n, exp)
	}
}

func TestAnyFinding(t *testing.T) {
	for name, tc := range map[string]struct {
		st   v1alpha1.SmbMountStatus
		want bool
	}{
		"healthy":         {v1alpha1.SmbMountStatus{Health: v1alpha1.HealthHealthy, Globalmount: &v1alpha1.GlobalmountStatus{Mounted: true, DirExists: true}}, false},
		"broken pod":      {v1alpha1.SmbMountStatus{PodMounts: []v1alpha1.PodMountStatus{{Health: v1alpha1.HealthBroken}}}, true},
		"broken gm":       {v1alpha1.SmbMountStatus{Health: v1alpha1.HealthBroken, Globalmount: &v1alpha1.GlobalmountStatus{Mounted: true}}, true},
		"shadow files":    {v1alpha1.SmbMountStatus{Globalmount: &v1alpha1.GlobalmountStatus{DirExists: true}}, true},
		"empty gm dir":    {v1alpha1.SmbMountStatus{Globalmount: &v1alpha1.GlobalmountStatus{DirExists: true, Empty: true}}, false},
		"orphan pod dir":  {v1alpha1.SmbMountStatus{OrphanPodDirs: []v1alpha1.OrphanPodDir{{Empty: false}}}, true},
		"empty orphan":    {v1alpha1.SmbMountStatus{OrphanPodDirs: []v1alpha1.OrphanPodDir{{Empty: true}}}, false},
		"suspect not yet": {v1alpha1.SmbMountStatus{PodMounts: []v1alpha1.PodMountStatus{{Health: v1alpha1.HealthSuspect}}}, false},
	} {
		if got := anyFinding([]v1alpha1.SmbMount{{Status: tc.st}}); got != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestSuppressionCondition(t *testing.T) {
	if c := suppressionCondition(nil, "", false); c != nil {
		t.Fatal("no condition when never suppressed")
	}
	c := suppressionCondition(nil, v1alpha1.SuppressionBackoff, true)
	if c.Status != metav1.ConditionTrue || c.Reason != "backoff" {
		t.Fatalf("%+v", c)
	}
	if c := suppressionCondition(c, "", false); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("a cleared suppression turns False: %+v", c)
	}
}

func TestStatusChangedIgnoresEmptyLists(t *testing.T) {
	a := &v1alpha1.SmbMountRemediationStatus{Stage: v1alpha1.StageDraining}
	b := a.DeepCopy()
	b.Transitions = []v1alpha1.StageTransition{}
	b.Scope = &v1alpha1.RemediationScope{}
	a.Scope = &v1alpha1.RemediationScope{PVs: nil}
	if ch, err := statusChanged(a, b); err != nil || ch {
		t.Fatalf("changed=%v err=%v", ch, err)
	}
	b.Stage = v1alpha1.StageReleasingMounts
	if ch, _ := statusChanged(a, b); !ch {
		t.Fatal("stage change not detected")
	}
}

func TestStripPVC(t *testing.T) {
	in := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "ns", ResourceVersion: "7", Labels: map[string]string{"a": "b"}},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}},
	}
	out, err := stripPVC(in)
	if err != nil {
		t.Fatal(err)
	}
	p := out.(*corev1.PersistentVolumeClaim)
	if p.Name != "data" || p.Namespace != "ns" || p.ResourceVersion != "7" || p.Spec.VolumeName != "pv" || p.Labels != nil || p.Spec.AccessModes != nil {
		t.Fatalf("%+v", p)
	}
	if o, _ := stripPVC("tombstone"); o != "tombstone" {
		t.Fatal("non-PVC objects pass through")
	}
}

func TestStageValues(t *testing.T) {
	for typ, want := range map[v1alpha1.RemediationType]int{v1alpha1.RemediationTypeWorkload: 12, v1alpha1.RemediationTypeSweep: 5} {
		got := stageValues[typ]
		if len(got) != want {
			t.Errorf("%s: %v", typ, got)
		}
		for _, s := range got {
			if s == string(v1alpha1.StageDone) || s == "" {
				t.Errorf("%s: unexpected value %q", typ, s)
			}
		}
	}
}
