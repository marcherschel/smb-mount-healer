package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group is the API group of all smb-mount-healer resources.
const Group = "smbhealer.herschel.io"

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

	// SchemeGroupVersion is an alias of GroupVersion expected by generated apply configurations.
	SchemeGroupVersion = GroupVersion

	// SchemeBuilder collects the functions that add this group-version to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	objectTypes []runtime.Object
)

// register is called from each kind's init().
func register(objs ...runtime.Object) { objectTypes = append(objectTypes, objs...) }

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, objectTypes...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Resource takes an unqualified resource and returns a Group qualified GroupResource.
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
