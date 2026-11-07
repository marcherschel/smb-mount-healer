package worker

// Worker RBAC (make manifests -> config/rbac/role-worker.yaml).

// +kubebuilder:rbac:groups="",resources=pods;persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbmounts,verbs=create;delete;get;list;watch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbmounts/status,verbs=patch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbhosttasks,verbs=get;list;watch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbhosttasks/status,verbs=update
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbquarantines,verbs=create;delete;get;list;watch
// +kubebuilder:rbac:groups=smbhealer.herschel.io,resources=smbquarantines/status,verbs=update
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch
