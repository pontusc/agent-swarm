package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
)

// markSyncFailed centralizes Synced=False condition updates for all reconcile
// failure paths so callers set a consistent type/reason/message shape.
func (r *RepositoryReconciler) markSyncFailed(ctx context.Context, repo *agentswarmv1alpha1.Repository, reason, message string) error {
	repo.Status.ObservedGeneration = repo.Generation
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               "Synced",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: repo.Generation,
	})
	if err := r.Status().Update(ctx, repo); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}
