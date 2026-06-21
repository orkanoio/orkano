package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Keys of the produced connection Secret — the frozen public contract an App's
// secretRef reads (ADR-0014). Only SecretKeyURI is load-bearing in v1; the rest
// are additive-safe to ship now. Renaming any key forces a version bump
// (ADR-0011), so they live here as the single source of truth the reconciler
// writes against — the api module is importable by third parties (ADR-0009), so
// a generator or the dashboard can build a secretRef without re-typing them.
const (
	SecretKeyURI      = "uri"
	SecretKeyHost     = "host"
	SecretKeyPort     = "port"
	SecretKeyDatabase = "database"
	SecretKeyUsername = "username"
	SecretKeyPassword = "password"
)

// PostgresSpec is the whole story: a name produces a database. Everything else
// (replicas/HA, backups, tuning, extra users, TLS, exposure) is a v2 dial that
// ADR-0011 lets us add additively later (ADR-0014).
type PostgresSpec struct {
	// Version is the PostgreSQL major series, resolved by the operator to a
	// digest-pinned postgres:<version> image. Immutable: a major upgrade needs
	// pg_upgrade/dump+restore (too sharp to automate in v1), so it is
	// delete-and-recreate — Apps survive it by referencing the connection
	// Secret by name (INV-03), not the running pod. The enum may be loosened
	// additively (ADR-0011), never tightened, so the operator must keep
	// shipping an image for every value while v1alpha1 is served.
	// +kubebuilder:validation:Enum="14";"15";"16";"17"
	// +kubebuilder:default="16"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="version is immutable; a major upgrade is delete-and-recreate"
	// +optional
	Version string `json:"version,omitempty"`

	// StorageSize is the data-directory PVC size. Grow-only, but enforced in
	// the reconciler rather than the schema — matching native PVC semantics,
	// where the apiserver accepts a shrink and only the controller/CSI rejects
	// it. A schema guard would then be frozen against loosening (ADR-0014).
	// +kubebuilder:default="10Gi"
	// +optional
	StorageSize *resource.Quantity `json:"storageSize,omitempty"`
}

type PostgresStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carry the single Ready summary (reasons Provisioning,
	// ProvisionFailed, Available) — the long-lived App/Domain shape, not
	// Build's run-to-completion phase.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// SecretName echoes the produced connection Secret's name (== metadata.name)
	// so kubectl describe and the dashboard surface the wiring. INV-03: only the
	// name ever appears here, never a value.
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=postgreses,categories=orkano
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Storage",type=string,JSONPath=`.spec.storageSize`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Postgres is the v1 service catalog: an engine-specific kind (not a generic
// Database{engine}), so its produced Secret carries an honest, frozen contract
// instead of a lowest-common-denominator one (ADR-0014). The operator renders a
// digest-pinned StatefulSet + Service + a connection Secret named exactly
// metadata.name in orkano-apps; Apps reference that Secret by name (INV-03).
type Postgres struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresSpec   `json:"spec,omitempty"`
	Status PostgresStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type PostgresList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Postgres `json:"items"`
}
