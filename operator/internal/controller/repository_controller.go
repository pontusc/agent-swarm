package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
	githubclient "github.com/pontuscurtsson/agent-swarm/operator/internal/github"
)

// RepositoryReconciler reconciles a Repository object.
//
// It is responsible for one thing per Repository CR: poll the upstream GitHub
// repo every spec.syncIntervalSeconds and reflect the set of open issues as
// child Issue CRs. The downstream Issue lifecycle (workspace, agent, PR) is
// owned by IssueReconciler — see issue_controller.go.
type RepositoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile polls GitHub for the Repository's open issues and materializes them
// as child Issue CRs. Each pass:
//
//  1. Loads GitHub App credentials from the referenced Secret.
//  2. Lists open issues via the GitHub API (paginated, PRs filtered out).
//  3. Creates or updates one Issue CR per open GitHub issue; prunes Issue CRs
//     whose upstream issue is no longer open.
//  4. Writes status (LastSyncTime, ObservedIssueCount, Synced condition).
//
// Requeues every Repository.spec.syncIntervalSeconds. Polling is the only
// trigger; webhooks are intentionally out of scope (see CLAUDE.md → Scope).
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var repo agentswarmv1alpha1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	creds, err := loadGitHubAppCreds(ctx, r.Client, repo.Namespace, repo.Spec.SecretRef.Name)
	if err != nil {
		if markErr := r.markSyncFailed(ctx, &repo, "SecretLoadError", err.Error()); markErr != nil {
			return ctrl.Result{}, fmt.Errorf("mark sync failed: %w (original error: %v)", markErr, err)
		}
		logger.Error(err, "Could not load GitHub App credentials", "secretName", repo.Spec.SecretRef.Name)
		return ctrl.Result{}, err
	}

	ghClient, err := githubclient.NewClient(creds)
	if err != nil {
		if markErr := r.markSyncFailed(ctx, &repo, "ClientInitError", err.Error()); markErr != nil {
			return ctrl.Result{}, fmt.Errorf("mark sync failed: %w (original error: %v)", markErr, err)
		}
		logger.Error(err, "Could not create GitHub client")
		return ctrl.Result{}, err
	}

	issues, err := ghClient.ListIssues(ctx, repo.Spec.Owner, repo.Spec.Repo)
	if err != nil {
		if markErr := r.markSyncFailed(ctx, &repo, "GitHubAPIError", err.Error()); markErr != nil {
			return ctrl.Result{}, fmt.Errorf("mark sync failed: %w (original error: %v)", markErr, err)
		}
		logger.Error(err, "Could not list repository issues", "owner", repo.Spec.Owner, "repo", repo.Spec.Repo)
		return ctrl.Result{}, err
	}
	if err := r.syncIssues(ctx, &repo, issues); err != nil {
		if markErr := r.markSyncFailed(ctx, &repo, "IssueSyncError", err.Error()); markErr != nil {
			return ctrl.Result{}, fmt.Errorf("mark sync failed: %w (original error: %v)", markErr, err)
		}
		logger.Error(err, "Could not sync child Issue resources")
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	repo.Status.ObservedGeneration = repo.Generation
	repo.Status.LastSyncTime = &now
	repo.Status.ObservedIssueCount = int32(len(issues))
	meta.SetStatusCondition(&repo.Status.Conditions, metav1.Condition{
		Type:               "Synced",
		Status:             metav1.ConditionTrue,
		Reason:             "SyncSucceeded",
		Message:            "Repository issues synced successfully",
		ObservedGeneration: repo.Generation,
	})

	if err := r.Status().Update(ctx, &repo); err != nil {
		if apierrors.IsConflict(err) {
			logger.V(1).Info("Repository status update conflicted, requeueing")
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "Could not update Repository status")
		return ctrl.Result{}, err
	}

	logger.Info("Synced Repository", "observedIssueCount", len(issues))
	return ctrl.Result{RequeueAfter: time.Duration(repo.Spec.SyncIntervalSeconds) * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentswarmv1alpha1.Repository{}).
		Owns(&agentswarmv1alpha1.Issue{}).
		Named("repository").
		Complete(r)
}
