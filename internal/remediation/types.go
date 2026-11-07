package remediation

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

// Config holds the remediation timings (see internal/config for env names).
type Config struct {
	DryRun                bool
	PollInterval          time.Duration // POLL_INTERVAL
	PlanningProbeWait     time.Duration // PLANNING_PROBE_WAIT
	ScaleDownWait         time.Duration // SCALE_DOWN_WAIT
	UnstageWait           time.Duration // UNSTAGE_WAIT
	QuarantineUnstageWait time.Duration // QUARANTINE_UNSTAGE_WAIT
	DriverRestartWait     time.Duration // DRIVER_RESTART_WAIT
	VerifyWait            time.Duration // VERIFY_WAIT
	VerifyNoPodGrace      time.Duration // VERIFY_NO_POD_GRACE
	EpisodeMaxDuration    time.Duration // EPISODE_MAX_DURATION
	BackoffBase           time.Duration // BACKOFF_BASE
	BackoffMax            time.Duration // BACKOFF_MAX
}

// WorkloadObservation is the live state of a scoped (or candidate) workload.
type WorkloadObservation struct {
	Ref           v1alpha1.WorkloadRef
	Exists        bool
	Replicas      int32 // spec.replicas (via scale)
	ReadyReplicas int32
}

// PodObservation is a pod on the remediation's node that uses an smb PV.
type PodObservation struct {
	Namespace   string
	Name        string
	UID         types.UID
	NodeName    string
	PVs         []string              // smb PVs the pod mounts, sorted
	Owner       *v1alpha1.WorkloadRef // resolved controller (Deployment/StatefulSet); nil if unowned
	Terminating bool
	Phase       corev1.PodPhase
}

// DriverPodObservation is a CSI node driver pod on the node.
type DriverPodObservation struct {
	Namespace string
	Name      string
	UID       types.UID
	Running   bool
	Ready     bool
}

// Observation is everything the control plane read for one Step.
type Observation struct {
	Now         time.Time
	SmbMounts   []v1alpha1.SmbMount // all SmbMounts of the node
	Workloads   []WorkloadObservation
	Pods        []PodObservation
	DriverPods  []DriverPodObservation
	HostTasks   []v1alpha1.SmbHostTask         // tasks owned by this remediation
	OtherActive []v1alpha1.SmbMountRemediation // non-terminal remediations on other nodes (scope conflicts)
	Aborted     bool                           // abort annotation is "true"
	Deleting    bool                           // deletionTimestamp set (finalizer rollback)
}

// Transition is one stage change produced by Step.
type Transition struct {
	From, To v1alpha1.Stage
	Reason   string
	Time     time.Time
}

// Result is the output of Step.
type Result struct {
	Status       v1alpha1.SmbMountRemediationStatus
	Commands     []command.ClusterCommand
	Transitions  []Transition
	RequeueAfter time.Duration
}

// SuppressionReason re-exports the API enum.
type SuppressionReason = v1alpha1.SuppressionReason

// TriggerInput is everything Trigger needs for one node.
type TriggerInput struct {
	NodeName     string
	Now          time.Time
	SmbMounts    []v1alpha1.SmbMount            // the node's SmbMounts
	Remediations []v1alpha1.SmbMountRemediation // all remediations (history + active, all nodes)
	Pods         []PodObservation               // pods on the node
}

// TriggerDecision says whether to create a remediation, and which SmbMounts were suppressed and why.
type TriggerDecision struct {
	Create     *v1alpha1.SmbMountRemediation // nil: nothing to do
	Suppressed map[string]SuppressionReason  // SmbMount name -> reason
}
