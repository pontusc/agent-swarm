package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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

// IssueReconciler reconciles an Issue object.
type IssueReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	newGitHubClient func(creds githubclient.AppCreds) (githubclient.Client, error)
}

// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create

const maxPrepRetries int32 = 3

// Reconcile prepares a per-issue workspace by creating a PVC and a prep Job
// that clones the repository and checks out a dedicated branch.
func (r *IssueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var issue agentswarmv1alpha1.Issue
	if err := r.Get(ctx, req.NamespacedName, &issue); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if issue.Status.Phase == agentswarmv1alpha1.IssuePhaseDone {
		if err := r.Delete(ctx, &issue); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete completed Issue %q: %w", req.NamespacedName.String(), err)
		}
		return ctrl.Result{}, nil
	}

	if issue.Status.Phase == agentswarmv1alpha1.IssuePhasePRCreated {
		repo, err := r.getOwningRepository(ctx, &issue)
		if err != nil {
			if markErr := r.markIssueFailed(ctx, &issue, "OwnerResolutionError", err.Error()); markErr != nil {
				return ctrl.Result{}, markErr
			}
			logger.Error(err, "Could not resolve owning Repository")
			return ctrl.Result{}, nil
		}
		return r.reconcilePullRequestStatus(ctx, &issue, repo)
	}

	if issue.Spec.State != agentswarmv1alpha1.IssueStateOpen {
		return ctrl.Result{}, nil
	}

	repo, err := r.getOwningRepository(ctx, &issue)
	if err != nil {
		if markErr := r.markIssueFailed(ctx, &issue, "OwnerResolutionError", err.Error()); markErr != nil {
			return ctrl.Result{}, markErr
		}
		logger.Error(err, "Could not resolve owning Repository")
		return ctrl.Result{}, nil
	}

	workspacePVC := workspacePVCName(&issue)
	branch := branchName(&issue)
	prepJobName := workspacePrepJobName(&issue)

	if err := r.ensureWorkspacePVC(ctx, &issue, workspacePVC); err != nil {
		if markErr := r.markIssueFailed(ctx, &issue, "WorkspacePVCError", err.Error()); markErr != nil {
			return ctrl.Result{}, markErr
		}
		return ctrl.Result{}, err
	}

	prepJob, err := r.ensureWorkspacePrepJob(ctx, &issue, repo, prepJobName, workspacePVC, branch)
	if err != nil {
		if markErr := r.markIssueFailed(ctx, &issue, "WorkspacePrepJobError", err.Error()); markErr != nil {
			return ctrl.Result{}, markErr
		}
		return ctrl.Result{}, err
	}

	issue.Status.ObservedGeneration = issue.Generation
	issue.Status.Branch = branch
	issue.Status.WorkspacePVC = workspacePVC
	issue.Status.PrepJobName = prepJobName

	if prepJob.Status.Succeeded > 0 {
		return r.reconcileAgent(ctx, &issue, repo, workspacePVC)
	}

	if prepJob.Status.Failed > 0 && jobReachedBackoffLimit(prepJob) {
		if prepJob.DeletionTimestamp != nil {
			issue.Status.Phase = agentswarmv1alpha1.IssuePhasePreparingWorkspace
			issue.Status.LastError = fmt.Sprintf("workspace prep failed, recreating job (%d/%d)", issue.Status.PrepRetries, maxPrepRetries)
			meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
				Type:               "WorkspacePrepared",
				Status:             metav1.ConditionFalse,
				Reason:             "WorkspaceRetrying",
				Message:            "Workspace preparation job is being recreated",
				ObservedGeneration: issue.Generation,
			})
			if err := r.updateIssueStatus(ctx, &issue); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		if issue.Status.PrepRetries < maxPrepRetries {
			issue.Status.PrepRetries++
			issue.Status.Phase = agentswarmv1alpha1.IssuePhasePreparingWorkspace
			issue.Status.LastError = fmt.Sprintf("workspace prep failed, retrying (%d/%d)", issue.Status.PrepRetries, maxPrepRetries)
			meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
				Type:               "WorkspacePrepared",
				Status:             metav1.ConditionFalse,
				Reason:             "WorkspaceRetrying",
				Message:            issue.Status.LastError,
				ObservedGeneration: issue.Generation,
			})
			if err := r.updateIssueStatus(ctx, &issue); err != nil {
				return ctrl.Result{}, err
			}

			if err := r.Delete(ctx, prepJob); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete failed workspace prep job %q: %w", prepJobName, err)
			}

			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}

		reason := "WorkspacePreparationFailed"
		message := fmt.Sprintf("workspace prep job %q failed after %d retries", prepJobName, maxPrepRetries)
		if markErr := r.markIssueFailed(ctx, &issue, reason, message); markErr != nil {
			return ctrl.Result{}, markErr
		}
		return ctrl.Result{}, nil
	}

	issue.Status.Phase = agentswarmv1alpha1.IssuePhasePreparingWorkspace
	issue.Status.LastError = ""
	meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "WorkspacePrepared",
		Status:             metav1.ConditionFalse,
		Reason:             "WorkspacePreparing",
		Message:            "Workspace preparation job is running",
		ObservedGeneration: issue.Generation,
	})
	if err := r.updateIssueStatus(ctx, &issue); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IssueReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentswarmv1alpha1.Issue{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Pod{}).
		Named("issue").
		Complete(r)
}
