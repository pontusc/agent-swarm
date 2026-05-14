// PR-tracking phase: once the publisher Job opens a pull request and writes
// the URL to Issue.status.prUrl, the Issue parks here in PRCreated. This
// file polls GitHub for the PR's merge state and transitions the Issue to
// Done (on merge) or Failed (on close-without-merge). Polling cadence is
// pullRequestStatusPollInterval; condition writes are change-gated so the
// 30s loop only touches the apiserver on real transitions.
package controller

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
	githubclient "github.com/pontuscurtsson/agent-swarm/operator/internal/github"
)

const pullRequestStatusPollInterval = 30 * time.Second

// reconcilePullRequestStatus polls GitHub for the tracked PR and marks the
// Issue as Done once the PR is merged.
func (r *IssueReconciler) reconcilePullRequestStatus(
	ctx context.Context,
	issue *agentswarmv1alpha1.Issue,
	repo *agentswarmv1alpha1.Repository,
) (ctrl.Result, error) {
	if issue.Status.PRURL == "" {
		message := "issue is in PRCreated phase but status.prUrl is empty"
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
		issue.Status.LastError = message
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "PullRequestCreated",
			Status:             metav1.ConditionFalse,
			Reason:             "PullRequestTrackingError",
			Message:            message,
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	prNumber, err := pullRequestNumberFromURL(issue.Status.PRURL)
	if err != nil {
		message := fmt.Sprintf("parse PR URL %q: %v", issue.Status.PRURL, err)
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
		issue.Status.LastError = message
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "PullRequestCreated",
			Status:             metav1.ConditionFalse,
			Reason:             "PullRequestTrackingError",
			Message:            message,
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	creds, err := loadGitHubAppCreds(ctx, r.Client, repo.Namespace, repo.Spec.SecretRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	ghClient, err := githubclient.NewClient(creds)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("create GitHub client: %w", err)
	}

	pr, err := ghClient.GetPullRequest(ctx, repo.Spec.Owner, repo.Spec.Repo, prNumber)
	if err != nil {
		return ctrl.Result{}, err
	}

	if pr.Merged {
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseDone
		issue.Status.LastError = ""
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "PullRequestCreated",
			Status:             metav1.ConditionTrue,
			Reason:             "PullRequestMerged",
			Message:            fmt.Sprintf("Pull request merged: %s", issue.Status.PRURL),
			ObservedGeneration: issue.Generation,
		})
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionTrue,
			Reason:             "IssueDone",
			Message:            "Issue workflow completed after pull request merge",
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if strings.EqualFold(pr.State, "closed") {
		message := fmt.Sprintf("pull request closed without merge: %s", issue.Status.PRURL)
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
		issue.Status.LastError = message
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "PullRequestCreated",
			Status:             metav1.ConditionFalse,
			Reason:             "PullRequestClosed",
			Message:            message,
			ObservedGeneration: issue.Generation,
		})
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "IssueNotDone",
			Message:            "Pull request closed before merge",
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	issue.Status.Phase = agentswarmv1alpha1.IssuePhasePRCreated
	changed := false
	changed = meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "PullRequestCreated",
		Status:             metav1.ConditionTrue,
		Reason:             "PullRequestOpen",
		Message:            fmt.Sprintf("Waiting for pull request merge: %s", issue.Status.PRURL),
		ObservedGeneration: issue.Generation,
	}) || changed
	changed = meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "Completed",
		Status:             metav1.ConditionFalse,
		Reason:             "WaitingForMerge",
		Message:            "Waiting for pull request to be merged",
		ObservedGeneration: issue.Generation,
	}) || changed

	if issue.Status.LastError != "" {
		issue.Status.LastError = ""
		changed = true
	}

	if changed {
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: pullRequestStatusPollInterval}, nil
}

// pullRequestNumberFromURL extracts the numeric PR id from a GitHub PR URL.
func pullRequestNumberFromURL(rawURL string) (int32, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return 0, err
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for idx := 0; idx+1 < len(segments); idx++ {
		if segments[idx] != "pull" {
			continue
		}

		number, err := strconv.ParseInt(segments[idx+1], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid pull request number %q", segments[idx+1])
		}

		return int32(number), nil
	}

	return 0, fmt.Errorf("no pull request number in URL path %q", parsed.Path)
}
