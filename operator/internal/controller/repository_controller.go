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

// RepositoryReconciler reconciles a Repository object
type RepositoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// newGitHubClient allows tests to inject a fake client and avoid network calls.
	newGitHubClient func(creds githubclient.AppCreds) (githubclient.Client, error)
}

// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Repository object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var repo agentswarmv1alpha1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	creds, err := r.loadCreds(ctx, &repo)
	if err != nil {
		if markErr := r.markSyncFailed(ctx, &repo, "SecretLoadError", err.Error()); markErr != nil {
			return ctrl.Result{}, fmt.Errorf("mark sync failed: %w (original error: %v)", markErr, err)
		}
		logger.Error(err, "Could not load GitHub App credentials", "secretName", repo.Spec.SecretRef.Name)
		return ctrl.Result{}, err
	}

	newGitHubClient := r.newGitHubClient
	if newGitHubClient == nil {
		// Production path: use the real GitHub App-backed client.
		newGitHubClient = githubclient.NewClient
	}

	ghClient, err := newGitHubClient(creds)
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
