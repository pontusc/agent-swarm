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
	// ObservedGeneration is the most recent .metadata.generation observed by the controller.
	// Clients can compare this to .metadata.generation to tell whether status reflects current spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastSyncTime is the timestamp of the last successful GitHub poll.
	// Nil if the Repository has never been synced.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// ObservedIssueCount is the number of open GitHub issues observed at the last successful sync.
	// +optional
	ObservedIssueCount int32 `json:"observedIssueCount,omitempty"`

	// Conditions represent the current state of the Repository resource.
	// Standard type: "Synced" (True after a successful poll; False with Reason set on error).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Repo",type="string",JSONPath=".spec.repo"
// +kubebuilder:printcolumn:name="Issues",type="integer",JSONPath=".status.observedIssueCount"
// +kubebuilder:printcolumn:name="Last-Sync",type="date",JSONPath=".status.lastSyncTime"

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