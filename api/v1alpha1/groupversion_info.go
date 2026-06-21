// Package v1alpha1 contains the orkano.io/v1alpha1 API types.
//
// +kubebuilder:object:generate=true
// +groupName=orkano.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "orkano.io", Version: "v1alpha1"}

	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&App{}, &AppList{},
		&Build{}, &BuildList{},
		&Domain{}, &DomainList{},
		&Postgres{}, &PostgresList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
