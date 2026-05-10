package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// IssueSpec defines the desired state of Issue.
//
// IssueSpec is a read-only mirror of a GitHub issue, written by RepositoryController.
type IssueSpec struct {
	// Number is the GitHub issue number within the parent repository.
	// +required
	// +kubebuilder:validation:Minimum=1
	Number int32 `json:"number"`

	// Title is the GitHub issue title.
	// +required
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title"`

	// Body is the GitHub issue body. May be empty.
	// +optional
	Body string `json:"body,omitempty"`

	// Labels mirrors the GitHub labels on the issue.
	// +optional
	Labels []string `json:"labels,omitempty"`

	// State is the GitHub issue state.
	// +required
	// +kubebuilder:validation:Enum=Open;Closed
	State IssueState `json:"state"`
}

// IssueState is one of Open, Closed.
type IssueState string

const (
	IssueStateOpen   IssueState = "Open"
	IssueStateClosed IssueState = "Closed"
)

// IssueStatus defines the observed state of Issue.
type IssueStatus struct {
	// ObservedGeneration is the most recent .metadata.generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase describes where this issue is in the execution pipeline.
	// +optional
	Phase IssuePhase `json:"phase,omitempty"`

	// Branch is the branch prepared for this issue's agent workflow.
	// +optional
	Branch string `json:"branch,omitempty"`

	// WorkspacePVC is the name of the PVC holding the per-issue workspace.
	// +optional
	WorkspacePVC string `json:"workspacePVC,omitempty"`

	// PrepJobName is the Job that clones the repository and creates the branch.
	// +optional
	PrepJobName string `json:"prepJobName,omitempty"`

	// PublishJobName is the Job that publishes mock agent output to GitHub.
	// +optional
	PublishJobName string `json:"publishJobName,omitempty"`

	// PrepRetries is how many times workspace preparation has been retried.
	// Max retries are controller-defined.
	// +optional
	PrepRetries int32 `json:"prepRetries,omitempty"`

	// AgentPodName will point to the assigned agent pod (Phase 2).
	// +optional
	AgentPodName string `json:"agentPodName,omitempty"`

	// PRURL will point to the pull request opened for this issue (Phase 2).
	// +optional
	PRURL string `json:"prUrl,omitempty"`

	// LastError is the latest reconciliation/runtime error for this issue.
	// +optional
	LastError string `json:"lastError,omitempty"`

	// Conditions represent the current state of the Issue resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IssuePhase is the high-level stage of issue handling.
//
// Current state machine:
// Pending -> PreparingWorkspace -> WorkspaceReady -> AgentRunning -> PublishPending -> PRCreated
// Any stage can transition to Failed.
//
// Planned later phases:
// PRCreated -> Done (on merge/close policy)
type IssuePhase string

const (
	IssuePhasePending            IssuePhase = "Pending"
	IssuePhasePreparingWorkspace IssuePhase = "PreparingWorkspace"
	IssuePhaseWorkspaceReady     IssuePhase = "WorkspaceReady"
	IssuePhaseAgentRunning       IssuePhase = "AgentRunning"
	IssuePhasePublishPending     IssuePhase = "PublishPending"
	IssuePhasePRCreated          IssuePhase = "PRCreated"
	IssuePhaseDone               IssuePhase = "Done"
	IssuePhaseFailed             IssuePhase = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Number",type="integer",JSONPath=".spec.number"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".spec.state"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Title",type="string",JSONPath=".spec.title",priority=1

// Issue is the Schema for the issues API
type Issue struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Issue
	// +required
	Spec IssueSpec `json:"spec"`

	// status defines the observed state of Issue
	// +optional
	Status IssueStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IssueList contains a list of Issue
type IssueList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Issue `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Issue{}, &IssueList{})
}
