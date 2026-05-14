// Package controller implements the agent-swarm reconcilers.
//
// IssueReconciler owns the per-issue lifecycle. Each Issue CR — created and
// kept in sync by RepositoryReconciler — is driven through a phase machine
// that:
//
//  1. Provisions a per-issue workspace (PVC + Job that clones the repo and
//     checks out a dedicated branch).
//  2. Runs an agent Pod against that workspace. The agent has no GitHub
//     credentials by design (see issue_controller_publish.go).
//  3. Hands the workspace to a publisher Job that signs in as the GitHub
//     App, pushes the branch, and opens a pull request.
//  4. Polls GitHub for the pull request's merge state.
//  5. Deletes the Issue CR on merge — ownerRefs cascade-delete the PVC,
//     Jobs and Pod. Agent log ConfigMaps are intentionally not owned so
//     they survive the cleanup (see persistAgentLogConfigMaps).
//
// Phase ↔ handler map (this file plus siblings):
//
//	Pending / PreparingWorkspace          → reconcileWorkspacePrep  (this file)
//	WorkspaceReady / AgentRunning         → reconcileAgent          (issue_controller_agent.go)
//	PublishPending                        → reconcilePublish        (issue_controller_publish.go)
//	PRCreated                             → reconcilePullRequestStatus (issue_controller_pr.go)
//	Done                                  → handleDone              (this file)
//	Failed                                → terminal; no-op until manual cleanup
//
// Any phase can transition to Failed via markIssueFailed
// (issue_controller_status.go); failIssue in this file is a thin convenience.
//
// Why a phase machine and not a chain of returns: each phase parks the
// reconciler with a RequeueAfter (or relies on Owns(...) watches for Pod/Job
// events to wake it). The phase is what tells the next reconcile *where to
// pick up*. Status writes that don't actually change anything are gated by
// setIfChanged (issue_controller_agent.go) to avoid API churn during long
// idle waits.
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
)

// IssueReconciler reconciles an Issue object.
//
// Fields:
//
//   - KubeClient is the typed clientset used for pod log streaming (the
//     controller-runtime client doesn't expose that). It's separate from the
//     embedded client.Client because the latter is a typed-cache-backed view
//     shared across the manager.
//   - AgentImage is the container image for the agent Pod created in
//     ensureAgentPod. Sourced from the AGENT_IMAGE env var in main.go so
//     deployments to environments other than the in-tree minikube registry
//     can override without rebuilding the operator binary.
type IssueReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	KubeClient kubernetes.Interface
	AgentImage string
}

// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create

// maxPrepRetries bounds how many times reconcileWorkspacePrep recreates a
// failed prep Job before marking the Issue Failed. The Job itself has its
// own backoffLimit (currently 1), so total Pod-level attempts are roughly
// (1 + Job.BackoffLimit) * maxPrepRetries.
const maxPrepRetries int32 = 3

// Reconcile is the IssueReconciler entry point. It loads the Issue and
// dispatches on .status.phase to the matching handler. Phases that are
// resolved by Pod/Job lifecycle events (AgentRunning, PublishPending) are
// not dispatched here — they are entered *from inside*
// reconcileWorkspacePrep via reconcileAgent → reconcilePublish, because
// reaching them requires Pod/Job state we'd have to fetch anyway. The
// dispatcher only handles phases that can be re-entered cold.
func (r *IssueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var issue agentswarmv1alpha1.Issue
	if err := r.Get(ctx, req.NamespacedName, &issue); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch issue.Status.Phase {
	case agentswarmv1alpha1.IssuePhaseDone:
		return r.handleDone(ctx, &issue, req.NamespacedName)
	case agentswarmv1alpha1.IssuePhasePRCreated:
		repo, err := r.getOwningRepository(ctx, &issue)
		if err != nil {
			return r.failIssue(ctx, &issue, "OwnerResolutionError", err)
		}
		return r.reconcilePullRequestStatus(ctx, &issue, repo)
	}

	// Spec.State mirrors GitHub. A closed-on-GitHub issue should not trigger
	// new work; closed-issue cleanup happens via RepositoryReconciler's prune
	// (see repository_controller_issue_sync.go).
	if issue.Spec.State != agentswarmv1alpha1.IssueStateOpen {
		return ctrl.Result{}, nil
	}

	repo, err := r.getOwningRepository(ctx, &issue)
	if err != nil {
		return r.failIssue(ctx, &issue, "OwnerResolutionError", err)
	}

	return r.reconcileWorkspacePrep(ctx, &issue, repo)
}

// handleDone garbage-collects an Issue that reached terminal success after a
// PR merge. Deleting the Issue cascades to the workspace PVC, prep and publish
// Jobs, and the agent Pod via ownerRefs. Agent log ConfigMaps survive because
// persistAgentLogConfigMaps deliberately omits the owner reference.
func (r *IssueReconciler) handleDone(ctx context.Context, issue *agentswarmv1alpha1.Issue, key types.NamespacedName) (ctrl.Result, error) {
	if err := r.Delete(ctx, issue); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete completed Issue %q: %w", key.String(), err)
	}
	return ctrl.Result{}, nil
}

// reconcileWorkspacePrep ensures the per-issue PVC and prep Job exist and
// drives the prep state machine. Outcomes:
//
//   - prep Job Succeeded → delegate to reconcileAgent (next phase).
//   - prep Job Failed past its BackoffLimit → handlePrepJobFailure decides
//     retry vs terminal Failed.
//   - Otherwise → markPrepInProgress and requeue after 10s.
//
// Most failures landing here are transient clone/network errors. "Repo does
// not exist" surfaces upstream in RepositoryReconciler's ListIssues call
// before we ever materialize an Issue CR.
func (r *IssueReconciler) reconcileWorkspacePrep(ctx context.Context, issue *agentswarmv1alpha1.Issue, repo *agentswarmv1alpha1.Repository) (ctrl.Result, error) {
	workspacePVC := workspacePVCName(issue)
	branch := branchName(issue)
	prepJobName := workspacePrepJobName(issue)

	if err := r.ensureWorkspacePVC(ctx, issue, workspacePVC); err != nil {
		return r.failIssue(ctx, issue, "WorkspacePVCError", err)
	}

	prepJob, err := r.ensureWorkspacePrepJob(ctx, issue, repo, prepJobName, workspacePVC, branch)
	if err != nil {
		return r.failIssue(ctx, issue, "WorkspacePrepJobError", err)
	}

	// Identifying status fields. These are stable for the lifetime of the
	// Issue so setIfChanged makes the Status().Update() a no-op once latched.
	scalarsChanged := setIfChanged(&issue.Status.ObservedGeneration, issue.Generation)
	scalarsChanged = setIfChanged(&issue.Status.Branch, branch) || scalarsChanged
	scalarsChanged = setIfChanged(&issue.Status.WorkspacePVC, workspacePVC) || scalarsChanged
	scalarsChanged = setIfChanged(&issue.Status.PrepJobName, prepJobName) || scalarsChanged

	if prepJob.Status.Succeeded > 0 {
		return r.reconcileAgent(ctx, issue, repo, workspacePVC)
	}

	if prepJob.Status.Failed > 0 && jobReachedBackoffLimit(prepJob) {
		return r.handlePrepJobFailure(ctx, issue, prepJob, prepJobName, scalarsChanged)
	}

	return r.markPrepInProgress(ctx, issue, scalarsChanged)
}

// handlePrepJobFailure reacts to a prep Job that exhausted its in-Job
// BackoffLimit by layering the controller's own bounded retry on top. Cases:
//
//   - Job already has a deletion timestamp: we kicked off the delete on a
//     previous reconcile; wait for the API to finalize before recreating.
//   - Retries left: increment PrepRetries, mark retrying, delete the Job
//     (ensureWorkspacePrepJob recreates it on the next reconcile), requeue.
//   - Retries exhausted: terminal Failed.
//
// Net retry budget is maxPrepRetries × (1 + Job.BackoffLimit). With the
// current BackoffLimit=1 and maxPrepRetries=3 that's up to 6 Pod attempts.
func (r *IssueReconciler) handlePrepJobFailure(ctx context.Context, issue *agentswarmv1alpha1.Issue, prepJob *batchv1.Job, prepJobName string, scalarsChanged bool) (ctrl.Result, error) {
	if prepJob.DeletionTimestamp != nil {
		lastError := fmt.Sprintf("workspace prep failed, recreating job (%d/%d)", issue.Status.PrepRetries, maxPrepRetries)
		return r.markPrepStatus(ctx, issue, "WorkspaceRetrying", "Workspace preparation job is being recreated", lastError, 5*time.Second, scalarsChanged)
	}

	if issue.Status.PrepRetries < maxPrepRetries {
		issue.Status.PrepRetries++
		msg := fmt.Sprintf("workspace prep failed, retrying (%d/%d)", issue.Status.PrepRetries, maxPrepRetries)
		// Always write — PrepRetries just bumped, so something changed.
		result, err := r.markPrepStatus(ctx, issue, "WorkspaceRetrying", msg, msg, 3*time.Second, true)
		if err != nil {
			return result, err
		}
		if err := r.Delete(ctx, prepJob); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete failed workspace prep job %q: %w", prepJobName, err)
		}
		return result, nil
	}

	message := fmt.Sprintf("workspace prep job %q failed after %d retries", prepJobName, maxPrepRetries)
	if err := r.markIssueFailed(ctx, issue, "WorkspacePreparationFailed", message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// markPrepInProgress reports the normal "prep job still running" state.
// Called every 10s while the prep Job runs; the underlying markPrepStatus
// gates the API write so identical reconciles don't generate traffic.
func (r *IssueReconciler) markPrepInProgress(ctx context.Context, issue *agentswarmv1alpha1.Issue, scalarsChanged bool) (ctrl.Result, error) {
	return r.markPrepStatus(ctx, issue, "WorkspacePreparing", "Workspace preparation job is running", "", 10*time.Second, scalarsChanged)
}

// markPrepStatus is the shared "still preparing" status write. It always
// flips Phase to PreparingWorkspace and the WorkspacePrepared condition to
// False, varying only in reason/message/lastError/requeue cadence.
//
// scalarsChanged is the change-tracking flag forwarded by the caller so
// the API write fires when any of (Phase, Conditions, LastError, ObservedGeneration,
// Branch, WorkspacePVC, PrepJobName) actually moved.
func (r *IssueReconciler) markPrepStatus(ctx context.Context, issue *agentswarmv1alpha1.Issue, reason, message, lastError string, requeueAfter time.Duration, scalarsChanged bool) (ctrl.Result, error) {
	changed := scalarsChanged
	changed = setIfChanged(&issue.Status.Phase, agentswarmv1alpha1.IssuePhasePreparingWorkspace) || changed
	changed = setIfChanged(&issue.Status.LastError, lastError) || changed
	changed = meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "WorkspacePrepared",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: issue.Generation,
	}) || changed

	if changed {
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// failIssue marks the Issue Failed and logs the cause. Returns a no-requeue
// result so we don't hot-loop on a permanent error. The Status update itself
// may still fail (e.g. apiserver conflict) — that error is surfaced so the
// reconciler retries naturally.
func (r *IssueReconciler) failIssue(ctx context.Context, issue *agentswarmv1alpha1.Issue, reason string, cause error) (ctrl.Result, error) {
	log.FromContext(ctx).Error(cause, "Issue reconciliation failed", "reason", reason)
	if markErr := r.markIssueFailed(ctx, issue, reason, cause.Error()); markErr != nil {
		return ctrl.Result{}, markErr
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
// Owns(Job, Pod) installs watches on the child resources so Job/Pod status
// changes wake the reconciler — this is what makes the phase machine
// progress without polling everything ourselves.
func (r *IssueReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentswarmv1alpha1.Issue{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Pod{}).
		Named("issue").
		Complete(r)
}
