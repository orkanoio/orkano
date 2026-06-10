// Package v1alpha1 contains the orkano.io/v1alpha1 API types.
//
// +kubebuilder:object:generate=true
// +groupName=orkano.io
package v1alpha1

import "k8s.io/apimachinery/pkg/runtime/schema"

var GroupVersion = schema.GroupVersion{Group: "orkano.io", Version: "v1alpha1"}
