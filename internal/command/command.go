package command

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// Command is any command.
type Command interface {
	// Key is a stable identity, e.g. "scale/Deployment/ns/name/0".
	Key() string
}

// ClusterCommand is executed by the control plane.
type ClusterCommand interface {
	Command
	isCluster()
}

// HostCommand is executed by a worker on its own node.
type HostCommand interface {
	Command
	isHost()
	// ID is the SmbHostTask action ID the result is recorded under.
	ID() string
}

// WorkloadAction is the metric label "action" of workload_actions_total.
type WorkloadAction string

const (
	ActionScaleDown     WorkloadAction = "scale_down"
	ActionScaleReassert WorkloadAction = "scale_reassert"
	ActionScaleUp       WorkloadAction = "scale_up"
	ActionDriverRestart WorkloadAction = "driver_restart"
	ActionRollback      WorkloadAction = "rollback"
)

// OwnerRef identifies the owning SmbMountRemediation (for ownerReferences).
type OwnerRef struct {
	Name string
	UID  types.UID
}

// ObjectRef identifies the object an Event is about.
type ObjectRef struct {
	Kind      string // SmbMountRemediation, SmbMount, Pod, Deployment, StatefulSet
	Namespace string // empty for cluster-scoped
	Name      string
	UID       types.UID
}

// Event types.
const (
	EventNormal  = "Normal"
	EventWarning = "Warning"
)

// --- cluster-side ---

// ScaleWorkload sets spec.replicas through the scale subresource. A no-op if
// replicas already match. Skipped (not an error) in dry-run mode.
type ScaleWorkload struct {
	Workload v1alpha1.WorkloadRef
	Replicas int32
	Action   WorkloadAction
}

// DeletePod deletes a pod with a UID precondition (driver restart). NotFound
// and UID-mismatch conflicts count as success.
type DeletePod struct {
	Namespace string
	Name      string
	UID       types.UID
	Action    WorkloadAction
}

// CreateHostTask creates an SmbHostTask (AlreadyExists counts as success),
// labelled with v1alpha1.LabelNode and LabelRemediation, owned by Owner.
type CreateHostTask struct {
	Name     string
	NodeName string
	Owner    OwnerRef
	DryRun   bool
	Actions  []v1alpha1.HostAction
}

// DeleteHostTask deletes an SmbHostTask (NotFound counts as success).
type DeleteHostTask struct {
	Name string
}

// EmitEvent records a Kubernetes Event. Messages must not contain entry
// names from quarantined data.
type EmitEvent struct {
	Regarding ObjectRef
	Type      string // EventNormal | EventWarning
	Reason    string
	Action    string
	Message   string
}

// PatchSmbMountRemediationMirror server-side-applies the control plane's
// status fields (FieldManagerControlPlane) on one SmbMount. Nil fields are
// left out of the apply (and thus released by the control plane manager).
type PatchSmbMountRemediationMirror struct {
	SmbMount             string
	Remediation          *v1alpha1.ActiveRemediation
	LastRemediation      *v1alpha1.LastRemediation
	RemediationsLastHour *int32
	// Conditions owned by the control plane: Remediating, Degraded, RemediationSuppressed.
	Conditions []metav1.Condition
}

// --- host-side ---

// ForceUnmount force-detaches Path if OnlyIf holds.
type ForceUnmount struct {
	ActionID string
	Path     string
	OnlyIf   v1alpha1.OnlyIf
}

// Quarantine moves the contents of Path to <QUARANTINE_DIR>/<PV>-<UTC> if
// OnlyIf holds and Path is not empty.
type Quarantine struct {
	ActionID string
	Path     string
	PV       string
	OnlyIf   v1alpha1.OnlyIf
}

// ProbeNow forces an immediate probe of Path (or of every mount on the node if Path is empty).
type ProbeNow struct {
	ActionID string
	Path     string
}

func (ScaleWorkload) isCluster()                  {}
func (DeletePod) isCluster()                      {}
func (CreateHostTask) isCluster()                 {}
func (DeleteHostTask) isCluster()                 {}
func (EmitEvent) isCluster()                      {}
func (PatchSmbMountRemediationMirror) isCluster() {}
func (ForceUnmount) isHost()                      {}
func (Quarantine) isHost()                        {}
func (ProbeNow) isHost()                          {}

func (c ScaleWorkload) Key() string {
	return fmt.Sprintf("scale/%s/%d", c.Workload, c.Replicas)
}
func (c DeletePod) Key() string {
	return "delete-pod/" + c.Namespace + "/" + c.Name + "/" + string(c.UID)
}
func (c CreateHostTask) Key() string { return "create-host-task/" + c.Name }
func (c DeleteHostTask) Key() string { return "delete-host-task/" + c.Name }
func (c EmitEvent) Key() string {
	return "event/" + c.Regarding.Kind + "/" + c.Regarding.Namespace + "/" + c.Regarding.Name + "/" + c.Reason
}
func (c PatchSmbMountRemediationMirror) Key() string { return "mirror/" + c.SmbMount }
func (c ForceUnmount) Key() string                   { return "force-unmount/" + c.ActionID }
func (c Quarantine) Key() string                     { return "quarantine/" + c.ActionID }
func (c ProbeNow) Key() string                       { return "probe-now/" + c.ActionID }

func (c ForceUnmount) ID() string { return c.ActionID }
func (c Quarantine) ID() string   { return c.ActionID }
func (c ProbeNow) ID() string     { return c.ActionID }

// FromHostAction converts an SmbHostTask action into its host command.
func FromHostAction(a v1alpha1.HostAction) (HostCommand, error) {
	switch a.Type {
	case v1alpha1.HostActionForceUnmount:
		return ForceUnmount{ActionID: a.ID, Path: a.Path, OnlyIf: a.OnlyIf}, nil
	case v1alpha1.HostActionQuarantine:
		if a.PV == "" {
			return nil, fmt.Errorf("action %q: Quarantine requires pv", a.ID)
		}
		return Quarantine{ActionID: a.ID, Path: a.Path, PV: a.PV, OnlyIf: a.OnlyIf}, nil
	case v1alpha1.HostActionProbeNow:
		return ProbeNow{ActionID: a.ID, Path: a.Path}, nil
	default:
		return nil, fmt.Errorf("action %q: unknown type %q", a.ID, a.Type)
	}
}
