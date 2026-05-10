package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
		logger.Error(err, "Could not update Repository status")
		return ctrl.Result{}, err
	}

	logger.Info("Synced Repository", "observedIssueCount", len(issues))
	return ctrl.Result{RequeueAfter: time.Duration(repo.Spec.SyncIntervalSeconds) * time.Second}, nil
}

// loadCreds reads the GitHub App credentials referenced by Repository.spec.secretRef.
// We parse app/installation IDs as integers because ghinstallation expects numeric IDs,
// while Secret values are always stored as bytes.
func (r *RepositoryReconciler) loadCreds(ctx context.Context, repo *agentswarmv1alpha1.Repository) (githubclient.AppCreds, error) {
	secretName := types.NamespacedName{Namespace: repo.Namespace, Name: repo.Spec.SecretRef.Name}

	var secret corev1.Secret
	if err := r.Get(ctx, secretName, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return githubclient.AppCreds{}, fmt.Errorf("secret %q not found", secretName.String())
		}
		return githubclient.AppCreds{}, fmt.Errorf("get secret %q: %w", secretName.String(), err)
	}

	appID, err := parseRequiredInt64(secret.Data, "appId")
	if err != nil {
		return githubclient.AppCreds{}, fmt.Errorf("secret %q: %w", secretName.String(), err)
	}

	installationID, err := parseRequiredInt64(secret.Data, "installationId")
	if err != nil {
		return githubclient.AppCreds{}, fmt.Errorf("secret %q: %w", secretName.String(), err)
	}

	privateKeyPEM, ok := secret.Data["privateKey"]
	if !ok {
		return githubclient.AppCreds{}, fmt.Errorf("missing key %q", "privateKey")
	}
	if len(privateKeyPEM) == 0 {
		return githubclient.AppCreds{}, fmt.Errorf("key %q must not be empty", "privateKey")
	}

	return githubclient.AppCreds{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKeyPEM:  privateKeyPEM,
	}, nil
}

// parseRequiredInt64 validates that a required Secret key exists and contains a
// base-10 integer value. This keeps Secret shape errors explicit and user-facing.
func parseRequiredInt64(data map[string][]byte, key string) (int64, error) {
	raw, ok := data[key]
	if !ok {
		return 0, fmt.Errorf("missing key %q", key)
	}

	value := strings.TrimSpace(string(raw))
	if value == "" {
		return 0, fmt.Errorf("key %q must not be empty", key)
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q as int64: %w", key, err)
	}

	return parsed, nil
}

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
	return r.Status().Update(ctx, repo)
}

// syncIssues materializes each fetched GitHub issue into an Issue CR.
func (r *RepositoryReconciler) syncIssues(ctx context.Context, repo *agentswarmv1alpha1.Repository, issues []githubclient.Issue) error {
	for _, issue := range issues {
		if err := r.upsertIssue(ctx, repo, issue); err != nil {
			return err
		}
	}
	return nil
}

// upsertIssue keeps one deterministic Issue CR (<repository-name>-<issue-number>)
// in sync with the latest GitHub issue payload.
func (r *RepositoryReconciler) upsertIssue(ctx context.Context, repo *agentswarmv1alpha1.Repository, issue githubclient.Issue) error {
	name := fmt.Sprintf("%s-%d", repo.Name, issue.Number)
	nn := types.NamespacedName{Namespace: repo.Namespace, Name: name}

	desiredSpec := agentswarmv1alpha1.IssueSpec{
		Number: issue.Number,
		Title:  issue.Title,
		Body:   issue.Body,
		Labels: issue.Labels,
		State:  toIssueState(issue.State),
	}

	var existing agentswarmv1alpha1.Issue
	err := r.Get(ctx, nn, &existing)
	if apierrors.IsNotFound(err) {
		issueCR := agentswarmv1alpha1.Issue{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: repo.Namespace,
			},
			Spec: desiredSpec,
		}
		if err := controllerutil.SetControllerReference(repo, &issueCR, r.Scheme); err != nil {
			return fmt.Errorf("set owner reference on Issue %q: %w", nn.String(), err)
		}
		if err := r.Create(ctx, &issueCR); err != nil {
			return fmt.Errorf("create Issue %q: %w", nn.String(), err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Issue %q: %w", nn.String(), err)
	}

	originalOwnerRefs := append([]metav1.OwnerReference(nil), existing.OwnerReferences...)
	if err := controllerutil.SetControllerReference(repo, &existing, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on Issue %q: %w", nn.String(), err)
	}

	ownerRefsChanged := !reflect.DeepEqual(existing.OwnerReferences, originalOwnerRefs)
	if reflect.DeepEqual(existing.Spec, desiredSpec) && !ownerRefsChanged {
		return nil
	}

	existing.Spec = desiredSpec
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update Issue %q: %w", nn.String(), err)
	}

	return nil
}

// toIssueState normalizes the GitHub client value to the CRD enum type.
func toIssueState(state string) agentswarmv1alpha1.IssueState {
	if state == string(agentswarmv1alpha1.IssueStateClosed) {
		return agentswarmv1alpha1.IssueStateClosed
	}
	return agentswarmv1alpha1.IssueStateOpen
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentswarmv1alpha1.Repository{}).
		Owns(&agentswarmv1alpha1.Issue{}).
		Named("repository").
		Complete(r)
}
