package controlplane

import (
	"context"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// Controller names (also the controller_runtime_* metric label "controller").
const (
	ControllerRemediation = "smbmountremediation"
	ControllerTrigger     = "smbmount-trigger"
	ControllerGC          = "smbmountremediation-gc"
)

// SetupWithManager registers the three controllers on mgr:
//
//   - RemediationReconciler (ControllerRemediation, one request per
//     remediation): SmbMountRemediations, plus their SmbHostTasks (label
//     remediation), SmbMounts and CSI driver pods of a remediation's node,
//     and scoped Deployments/StatefulSets (metadata only) mapped to the
//     active remediations they concern.
//   - TriggerController (ControllerTrigger, one request per node):
//     SmbMounts, and remediation changes (which may lift a suppression on
//     any node).
//   - GCController (ControllerGC, one request per node): remediations.
//
// Every controller runs one worker, so a node's decisions are serialized.
func (e *Engine) SetupWithManager(mgr ctrl.Manager, skipNameValidation bool) error {
	opts := controller.Options{SkipNameValidation: ptr.To(skipNameValidation)}
	r := mgr.GetClient()

	err := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerRemediation).
		For(&v1alpha1.SmbMountRemediation{}).
		Watches(&v1alpha1.SmbHostTask{}, handler.EnqueueRequestsFromMapFunc(taskToRemediation)).
		Watches(&v1alpha1.SmbMount{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			sm, ok := o.(*v1alpha1.SmbMount)
			if !ok {
				return nil
			}
			return activeOnNode(ctx, r, sm.Spec.NodeName)
		})).
		// The pod informer is restricted to the driver namespace and label
		// selector (see NewManager), so these are CSI node driver pods only.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			p, ok := o.(*corev1.Pod)
			if !ok || p.Spec.NodeName == "" {
				return nil
			}
			return activeOnNode(ctx, r, p.Spec.NodeName)
		})).
		WatchesMetadata(&appsv1.Deployment{}, workloadHandler(r, v1alpha1.WorkloadKindDeployment)).
		WatchesMetadata(&appsv1.StatefulSet{}, workloadHandler(r, v1alpha1.WorkloadKindStatefulSet)).
		WithOptions(opts).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			return e.ReconcileRemediation(ctx, req.Name)
		}))
	if err != nil {
		return err
	}

	err = ctrl.NewControllerManagedBy(mgr).
		Named(ControllerTrigger).
		Watches(&v1alpha1.SmbMount{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			sm, ok := o.(*v1alpha1.SmbMount)
			if !ok || sm.Spec.NodeName == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: sm.Spec.NodeName}}}
		})).
		Watches(&v1alpha1.SmbMountRemediation{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			return allNodes(ctx, r, o)
		})).
		WithOptions(opts).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			return e.ReconcileTrigger(ctx, req.Name)
		}))
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerGC).
		Watches(&v1alpha1.SmbMountRemediation{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			rem, ok := o.(*v1alpha1.SmbMountRemediation)
			if !ok || rem.Spec.NodeName == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: rem.Spec.NodeName}}}
		})).
		WithOptions(opts).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			return e.ReconcileGC(ctx, req.Name)
		}))
}

func taskToRemediation(_ context.Context, o client.Object) []reconcile.Request {
	if n := o.GetLabels()[v1alpha1.LabelRemediation]; n != "" {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: n}}}
	}
	for _, ref := range o.GetOwnerReferences() {
		if ref.Kind == "SmbMountRemediation" {
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: ref.Name}}}
		}
	}
	return nil
}

func listRemediations(ctx context.Context, r client.Reader) []v1alpha1.SmbMountRemediation {
	var list v1alpha1.SmbMountRemediationList
	if err := r.List(ctx, &list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list remediations for event mapping")
		return nil
	}
	return list.Items
}

func activeOnNode(ctx context.Context, r client.Reader, node string) []reconcile.Request {
	if node == "" {
		return nil
	}
	var out []reconcile.Request
	for _, rem := range listRemediations(ctx, r) {
		if rem.Spec.NodeName == node && !rem.Status.Stage.Terminal() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: rem.Name}})
		}
	}
	return out
}

func workloadHandler(r client.Reader, kind v1alpha1.WorkloadKind) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		ref := v1alpha1.WorkloadRef{Kind: kind, Namespace: o.GetNamespace(), Name: o.GetName()}
		var out []reconcile.Request
		for _, rem := range listRemediations(ctx, r) {
			if rem.Status.Stage.Terminal() || rem.Status.Scope == nil {
				continue
			}
			for _, w := range rem.Status.Scope.Workloads {
				if w.WorkloadRef == ref {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: rem.Name}})
					break
				}
			}
		}
		return out
	})
}

// allNodes maps a remediation change to every node with SmbMounts (a
// finished remediation can lift a backoff or scope-conflict suppression
// anywhere) plus the remediation's own node.
func allNodes(ctx context.Context, r client.Reader, o client.Object) []reconcile.Request {
	nodes := map[string]bool{}
	if rem, ok := o.(*v1alpha1.SmbMountRemediation); ok && rem.Spec.NodeName != "" {
		nodes[rem.Spec.NodeName] = true
	}
	var list v1alpha1.SmbMountList
	if err := r.List(ctx, &list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list SmbMounts for event mapping")
	}
	for _, sm := range list.Items {
		if sm.Spec.NodeName != "" {
			nodes[sm.Spec.NodeName] = true
		}
	}
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]reconcile.Request, 0, len(names))
	for _, n := range names {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: n}})
	}
	return out
}
