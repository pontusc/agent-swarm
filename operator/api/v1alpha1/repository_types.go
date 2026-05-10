package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RepositorySpec defines the desired state of Repository.
type RepositorySpec struct {
	// Owner is the GitHub owner (user or org) of the repository to sync.
	// +required
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`

	// Repo is the GitHub repository name (the part after `<owner>/`).
	// +required
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// SyncIntervalSeconds is how often to poll GitHub for issues.
	// +optional
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=30
	SyncIntervalSeconds int32 `json:"syncIntervalSeconds,omitempty"`

	// SecretRef points to a Secret in the same namespace carrying GitHub App
	// credentials. Required keys: appId, installationId, privateKey.
	// +required
	SecretRef LocalSecretReference `json:"secretRef"`
}

// LocalSecretReference references a Secret in the same namespace as the referrer.
type LocalSecretReference struct {
	// Name of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// RepositoryStatus defines the observed state of Repository.
type RepositoryStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Repository resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Repository is the Schema for the repositories API
type Repository struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Repository
	// +required
	Spec RepositorySpec `json:"spec"`

	// status defines the observed state of Repository
	// +optional
	Status RepositoryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RepositoryList contains a list of Repository
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Repository `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Repository{}, &RepositoryList{})
}