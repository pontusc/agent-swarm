package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
)

// markIssueFailed centralizes status/condition updates for terminal prep failures.
func (r *IssueReconciler) markIssueFailed(ctx context.Context, issue *agentswarmv1alpha1.Issue, reason string, message string) error {
	issue.Status.ObservedGeneration = issue.Generation
	issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
	issue.Status.LastError = message
	meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "WorkspacePrepared",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: issue.Generation,
	})

	return r.updateIssueStatus(ctx, issue)
}

// updateIssueStatus writes status and treats conflicts as retryable/noisy updates.
func (r *IssueReconciler) updateIssueStatus(ctx context.Context, issue *agentswarmv1alpha1.Issue) error {
	if err := r.Status().Update(ctx, issue); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}
