//go:build envtest

package v1alpha1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	ac "github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
)

// TestCRDsOnEnvtest installs config/crd/bases into a real API server and
// exercises every kind: create, status subresource, immutability, enum
// validation, printer columns (what `kubectl get` shows) and SSA field
// ownership on SmbMount status.
func TestCRDsOnEnvtest(t *testing.T) {
	g := NewWithT(t)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest (run via `make test-envtest`): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme, err := kube.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(apiextensionsv1.AddToScheme(scheme)).To(Succeed())
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))

	t.Run("CRD metadata", func(t *testing.T) {
		g := NewWithT(t)
		want := map[string]struct {
			short   string
			columns []string
		}{
			"smbmounts":            {"smbm", []string{"Node", "PV", "Health", "Reason", "Stage", "Last Outcome", "Age"}},
			"smbmountremediations": {"smbr", []string{"Node", "Type", "Stage", "Outcome", "Dry-Run", "Age"}},
			"smbhosttasks":         {"smbt", []string{"Node", "Phase", "Dry-Run", "Age"}},
			"smbquarantines":       {"smbq", []string{"Node", "PV", "Files", "Size", "Quarantined", "Last Scan"}},
		}
		for plural, w := range want {
			var crd apiextensionsv1.CustomResourceDefinition
			g.Expect(c.Get(ctx, client.ObjectKey{Name: plural + "." + v1.Group}, &crd)).To(Succeed())
			g.Expect(crd.Spec.Scope).To(Equal(apiextensionsv1.ClusterScoped))
			g.Expect(crd.Spec.Names.ShortNames).To(ConsistOf(w.short))
			g.Expect(crd.Spec.Names.Categories).To(ContainElement("smbhealer"))
			g.Expect(crd.Spec.Versions).To(HaveLen(1))
			g.Expect(crd.Spec.Versions[0].Subresources.Status).NotTo(BeNil())
			var cols []string
			for _, col := range crd.Spec.Versions[0].AdditionalPrinterColumns {
				cols = append(cols, col.Name)
			}
			g.Expect(cols).To(Equal(w.columns), plural)
		}
	})

	t.Run("SmbMount", func(t *testing.T) {
		g := NewWithT(t)
		name := v1.SmbMountName("node-a", "storagebox-x")
		obj := &v1.SmbMount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{v1.LabelNode: "node-a", v1.LabelPV: "storagebox-x"}},
			Spec: v1.SmbMountSpec{
				NodeName: "node-a", PVName: "storagebox-x", Share: "//samba/x", VolumeHandle: "storagebox-x",
				GlobalmountPath: "/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/abc/globalmount",
			},
		}
		g.Expect(c.Create(ctx, obj)).To(Succeed())

		// Worker applies the health part.
		worker := ac.SmbMount(name).WithStatus(ac.SmbMountStatus().
			WithHealth(v1.HealthBroken).WithReason(v1.ReasonWrongFSType).WithLastProbeTime(now).
			WithDebounce(ac.Debounce().WithConsecutiveFailures(3).WithFirstFailureTime(now).WithLastClass(v1.ReasonWrongFSType)).
			WithGlobalmount(ac.GlobalmountStatus().WithMounted(true).WithFSType("xfs").WithDirExists(true).WithEmpty(false)).
			WithPodMounts(ac.PodMountStatus().WithNamespace("media").WithPod("app-0").WithUID("uid-1").WithMounted(true).
				WithFSType("xfs").WithHealth(v1.HealthBroken).WithReason(v1.ReasonWrongFSType)).
			WithConditions(metav1ConditionAC(v1.ConditionHealthy, metav1.ConditionFalse, "WrongFSType", now)))
		g.Expect(c.Status().Apply(ctx, worker, client.FieldOwner(v1.FieldManagerWorker), client.ForceOwnership)).To(Succeed())

		// Control plane applies the remediation part.
		cp := ac.SmbMount(name).WithStatus(ac.SmbMountStatus().
			WithRemediation(ac.ActiveRemediation().WithActive(true).WithName("node-a-1").WithStage(v1.StageDraining).WithStageEnteredTime(now)).
			WithConditions(metav1ConditionAC(v1.ConditionRemediating, metav1.ConditionTrue, "Draining", now)))
		g.Expect(c.Status().Apply(ctx, cp, client.FieldOwner(v1.FieldManagerControlPlane), client.ForceOwnership)).To(Succeed())

		// Worker re-applies only its fields; the control plane's survive.
		worker2 := ac.SmbMount(name).WithStatus(ac.SmbMountStatus().
			WithHealth(v1.HealthHealthy).WithLastProbeTime(now).
			WithConditions(metav1ConditionAC(v1.ConditionHealthy, metav1.ConditionTrue, "Healthy", now)))
		g.Expect(c.Status().Apply(ctx, worker2, client.FieldOwner(v1.FieldManagerWorker), client.ForceOwnership)).To(Succeed())

		var got v1.SmbMount
		g.Expect(c.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
		g.Expect(got.Status.Health).To(Equal(v1.HealthHealthy))
		g.Expect(got.Status.Reason).To(BeEmpty())
		g.Expect(got.Status.PodMounts).To(BeEmpty())
		g.Expect(got.Status.Remediation).NotTo(BeNil())
		g.Expect(got.Status.Remediation.Stage).To(Equal(v1.StageDraining))
		var condTypes []string
		for _, cnd := range got.Status.Conditions {
			condTypes = append(condTypes, cnd.Type)
		}
		g.Expect(condTypes).To(ConsistOf(v1.ConditionHealthy, v1.ConditionRemediating))

		// Spec is immutable.
		got.Spec.Share = "//elsewhere/x"
		err := c.Update(ctx, &got)
		g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "spec update must be rejected: %v", err)

		// Printer columns as kubectl sees them.
		table := getTable(t, cfg, "smbmounts")
		g.Expect(table.columns).To(Equal([]string{"Name", "Node", "PV", "Health", "Reason", "Stage", "Last Outcome", "Age"}))
		g.Expect(table.rows).To(ContainElement(HaveExactElements(name, "node-a", "storagebox-x", "Healthy", nil, "draining", nil, Not(BeEmpty()))))
	})

	t.Run("SmbMountRemediation", func(t *testing.T) {
		g := NewWithT(t)
		name := v1.RemediationName("node-a", now.Time)
		obj := &v1.SmbMountRemediation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{v1.FinalizerRestoreReplicas}},
			Spec: v1.SmbMountRemediationSpec{
				NodeName: "node-a", Type: v1.RemediationTypeWorkload,
				Trigger: v1.RemediationTrigger{SmbMounts: []string{"node-a.storagebox-x"}, Reasons: []v1.TriggerReason{v1.TriggerWrongFSType}},
			},
		}
		g.Expect(c.Create(ctx, obj)).To(Succeed())
		obj.Status = v1.SmbMountRemediationStatus{
			Stage: v1.StageDone, StageEnteredTime: &now, StartedTime: &now, Deadline: &now,
			Scope: &v1.RemediationScope{
				Workloads: []v1.ScopedWorkload{{WorkloadRef: v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: "media", Name: "app"}, OriginalReplicas: 1, State: v1.WorkloadRestored}},
				PVs:       []string{"storagebox-x"},
				DrainPods: []v1.PodRef{{Namespace: "media", Name: "app-0", UID: "uid-1"}},
			},
			DeadGlobalmounts: []v1.DeadGlobalmount{{PVName: "storagebox-x", Path: "/var/lib/kubelet/x/globalmount"}},
			Driver:           &v1.DriverRestart{OldPodName: "csi-smb-node-1", OldPodUID: "d1", NewPodUID: "d2"},
			HostTasks:        []v1.HostTaskRef{{Name: v1.HostTaskName(name, v1.StageQuarantining, 1), Stage: v1.StageQuarantining, Phase: v1.HostTaskSucceeded}},
			Verification:     []v1.WorkloadVerification{{Workload: v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: "media", Name: "app"}, Result: v1.VerificationVerified}},
			Outcome:          v1.OutcomeHealed,
			Transitions:      []v1.StageTransition{{From: v1.StagePending, To: v1.StagePlanning, Reason: "no_conflict", Time: now}},
		}
		g.Expect(c.Status().Update(ctx, obj)).To(Succeed())

		bad := obj.DeepCopy()
		bad.Status.Stage = "exploding"
		g.Expect(apierrors.IsInvalid(c.Status().Update(ctx, bad))).To(BeTrue(), "unknown stage must be rejected")

		tooMany := obj.DeepCopy()
		for i := 0; i < v1.MaxTransitions; i++ {
			tooMany.Status.Transitions = append(tooMany.Status.Transitions, tooMany.Status.Transitions[0])
		}
		g.Expect(apierrors.IsInvalid(c.Status().Update(ctx, tooMany))).To(BeTrue(), "more than 50 transitions must be rejected")

		table := getTable(t, cfg, "smbmountremediations")
		g.Expect(table.columns).To(Equal([]string{"Name", "Node", "Type", "Stage", "Outcome", "Dry-Run", "Age"}))
		g.Expect(table.rows).To(ContainElement(HaveExactElements(name, "node-a", "Workload", "done", "healed", BeNil(), Not(BeEmpty()))))

		// Finalizer keeps a deleted remediation around until the control plane releases it.
		g.Expect(c.Delete(ctx, obj)).To(Succeed())
		var got v1.SmbMountRemediation
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), &got)).To(Succeed())
		g.Expect(got.DeletionTimestamp).NotTo(BeNil())
		got.Finalizers = nil
		g.Expect(c.Update(ctx, &got)).To(Succeed())
	})

	t.Run("SmbHostTask", func(t *testing.T) {
		g := NewWithT(t)
		obj := &v1.SmbHostTask{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a-1-quarantining-1", Labels: map[string]string{v1.LabelNode: "node-a", v1.LabelRemediation: "node-a-1"}},
			Spec: v1.SmbHostTaskSpec{NodeName: "node-a", DryRun: true, Actions: []v1.HostAction{
				{ID: "u1", Type: v1.HostActionForceUnmount, Path: "/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pv/mount", OnlyIf: v1.OnlyIfNotHealthyMount},
				{ID: "q1", Type: v1.HostActionQuarantine, Path: "/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pv/mount", PV: "pv", OnlyIf: v1.OnlyIfNotMountpoint},
				{ID: "p1", Type: v1.HostActionProbeNow, Path: "/var/lib/kubelet/x"},
			}},
		}
		g.Expect(c.Create(ctx, obj)).To(Succeed())
		obj.Status = v1.SmbHostTaskStatus{Phase: v1.HostTaskSucceeded, ObservedGeneration: obj.Generation, Results: []v1.HostActionResult{
			{ID: "u1", Result: v1.HostActionDryRun, Time: now},
			{ID: "q1", Result: v1.HostActionDryRun, Destination: "/var/lib/kubelet/smb-mount-healer-quarantine/pv-20261011T070809Z", Entries: 2, Time: now},
			{ID: "p1", Result: v1.HostActionSucceeded, Time: now},
		}}
		g.Expect(c.Status().Update(ctx, obj)).To(Succeed())

		badType := &v1.SmbHostTask{ObjectMeta: metav1.ObjectMeta{Name: "bad-type"}, Spec: v1.SmbHostTaskSpec{NodeName: "n", Actions: []v1.HostAction{{ID: "x", Type: "Delete", Path: "/x"}}}}
		g.Expect(apierrors.IsInvalid(c.Create(ctx, badType))).To(BeTrue(), "unknown action type must be rejected")
		relPath := &v1.SmbHostTask{ObjectMeta: metav1.ObjectMeta{Name: "rel-path"}, Spec: v1.SmbHostTaskSpec{NodeName: "n", Actions: []v1.HostAction{{ID: "x", Type: v1.HostActionProbeNow, Path: "x"}}}}
		g.Expect(apierrors.IsInvalid(c.Create(ctx, relPath))).To(BeTrue(), "relative path must be rejected")
		noActions := &v1.SmbHostTask{ObjectMeta: metav1.ObjectMeta{Name: "no-actions"}, Spec: v1.SmbHostTaskSpec{NodeName: "n"}}
		g.Expect(apierrors.IsInvalid(c.Create(ctx, noActions))).To(BeTrue(), "empty actions must be rejected")

		table := getTable(t, cfg, "smbhosttasks")
		g.Expect(table.columns).To(Equal([]string{"Name", "Node", "Phase", "Dry-Run", "Age"}))
		g.Expect(table.rows).To(ContainElement(HaveExactElements(obj.Name, "node-a", "Succeeded", true, Not(BeEmpty()))))
	})

	t.Run("SmbQuarantine", func(t *testing.T) {
		g := NewWithT(t)
		folder := "storagebox-transmission-config-20261011T070809Z"
		obj := &v1.SmbQuarantine{
			ObjectMeta: metav1.ObjectMeta{Name: v1.SmbQuarantineName("node-a", folder), Labels: map[string]string{v1.LabelNode: "node-a"}},
			Spec: v1.SmbQuarantineSpec{
				NodeName: "node-a", Path: "/var/lib/kubelet/smb-mount-healer-quarantine/" + folder,
				PVName: "storagebox-transmission-config", QuarantinedTime: now,
			},
		}
		g.Expect(c.Create(ctx, obj)).To(Succeed())
		obj.Status = v1.SmbQuarantineStatus{
			Entries: 3, Files: 214, Bytes: 1288490188, SizeTruncated: false,
			OldestModTime: &now, NewestModTime: &now, LastScanTime: &now,
			Conditions: []metav1.Condition{{Type: v1.ConditionPending, Status: metav1.ConditionTrue, Reason: "DataPresent", Message: "quarantined data is still present", LastTransitionTime: now}},
		}
		g.Expect(c.Status().Update(ctx, obj)).To(Succeed())

		neg := obj.DeepCopy()
		neg.Status.Files = -1
		g.Expect(apierrors.IsInvalid(c.Status().Update(ctx, neg))).To(BeTrue(), "negative counts must be rejected")

		table := getTable(t, cfg, "smbquarantines")
		g.Expect(table.columns).To(Equal([]string{"Name", "Node", "PV", "Files", "Size", "Quarantined", "Last Scan"}))
		g.Expect(table.rows).To(ContainElement(HaveExactElements(obj.Name, "node-a", "storagebox-transmission-config",
			BeNumerically("==", 214), BeNumerically("==", 1288490188), Not(BeEmpty()), Not(BeEmpty()))))
	})
}

func metav1ConditionAC(typ string, status metav1.ConditionStatus, reason string, t metav1.Time) *metav1ac.ConditionApplyConfiguration {
	return metav1ac.Condition().WithType(typ).WithStatus(status).WithReason(reason).WithMessage(reason).WithLastTransitionTime(t)
}

type tableResult struct {
	columns []string
	rows    [][]any
}

// getTable lists a resource the way kubectl does (server-side Table).
func getTable(t *testing.T, cfg *rest.Config, plural string) tableResult {
	t.Helper()
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(cfg.Host, "/")+"/apis/"+v1.Group+"/v1alpha1/"+plural, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (%s) table: %s: %s", plural, req.URL, resp.Status, body)
	}
	var tbl metav1.Table
	if err := json.Unmarshal(body, &tbl); err != nil {
		t.Fatal(err)
	}
	var res tableResult
	for _, c := range tbl.ColumnDefinitions {
		res.columns = append(res.columns, c.Name)
	}
	for _, r := range tbl.Rows {
		res.rows = append(res.rows, slices.Clone(r.Cells))
	}
	return res
}
