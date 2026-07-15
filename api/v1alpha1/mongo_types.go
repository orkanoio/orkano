package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MongoSpec is intentionally the same small shape as PostgresSpec: a name
// produces one persistent database, while engine-specific upgrades and tuning
// stay explicit instead of hiding behind a generic Database abstraction.
type MongoSpec struct {
	// Version is the MongoDB major release series resolved to a digest-pinned
	// mongo image. Immutable: changing release series needs an explicit data
	// migration, so v1 uses delete-and-recreate rather than pretending it is safe.
	// +kubebuilder:default="8.0"
	// +kubebuilder:validation:Enum="8.0"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="version is immutable; migrate data into a new Mongo resource to change it"
	Version string `json:"version,omitempty"`

	// StorageSize is the requested data PVC size. It may grow; the reconciler
	// rejects shrink requests because Kubernetes volume expansion is one-way.
	// +kubebuilder:default="10Gi"
	StorageSize *resource.Quantity `json:"storageSize,omitempty"`
}

type MongoStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	// SecretName is the connection Secret produced for this database. It carries
	// only the name, never any credential value.
	SecretName string `json:"secretName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=mongoes,scope=Namespaced,categories=orkano
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Storage",type=string,JSONPath=`.spec.storageSize`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Mongo is Orkano's MongoDB catalog resource. The operator owns its
// StatefulSet, headless Service, data PVC, and connection Secret.
type Mongo struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MongoSpec   `json:"spec,omitempty"`
	Status MongoStatus `json:"status,omitempty"`
}

func (m *Mongo) ReadyCondition() *metav1.Condition {
	for i := range m.Status.Conditions {
		if m.Status.Conditions[i].Type == ConditionReady {
			return &m.Status.Conditions[i]
		}
	}
	return nil
}

func (m *Mongo) IsReady() bool {
	c := m.ReadyCondition()
	return c != nil && c.Status == metav1.ConditionTrue
}

func (m *Mongo) ConnectionSecretRef() *corev1.SecretReference {
	if m.Status.SecretName == "" {
		return nil
	}
	return &corev1.SecretReference{Name: m.Status.SecretName, Namespace: m.Namespace}
}

// +kubebuilder:object:root=true

type MongoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Mongo `json:"items"`
}
