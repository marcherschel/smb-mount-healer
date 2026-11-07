package controlplane

// Control-plane RBAC (make manifests -> config/rbac/role-controlplane.yaml).
// Leader-election Lease access is a namespaced Role in the Helm chart
// (production/smb-mount-healer/templates/rbac/leader-election.yaml).

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale;statefulsets/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbmounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbmounts/status,verbs=patch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbhosttasks,verbs=create;delete;get;list;watch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbmountremediations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbmountremediations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbmountremediations/finalizers,verbs=update
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbquarantines,verbs=get;list;watch
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch
