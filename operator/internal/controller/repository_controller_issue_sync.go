package controller

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
	githubclient "github.com/pontuscurtsson/agent-swarm/operator/internal/github"
)

// syncIssues materializes each fetched GitHub issue into an Issue CR.
func (r *RepositoryReconciler) syncIssues(ctx context.Context, repo *agentswarmv1alpha1.Repository, issues []githubclient.Issue) error {
	desiredNames := make(map[string]struct{}, len(issues))

	for _, issue := range issues {
		if toIssueState(issue.State) != agentswarmv1alpha1.IssueStateOpen {
			continue
		}

		name := fmt.Sprintf("%s-%d", repo.Name, issue.Number)
		desiredNames[name] = struct{}{}

		if err := r.upsertIssue(ctx, repo, issue); err != nil {
			return err
		}
	}

	if err := r.pruneOutdatedIssues(ctx, repo, desiredNames); err != nil {
		return err
	}

	return nil
}

// upsertIssue keeps one deterministic Issue CR (<repository-name>-<issue-number>)
// in sync with the latest GitHub issue payload.
func (r *RepositoryReconciler) upsertIssue(ctx context.Context, repo *agentswarmv1alpha1.Repository, issue githubclient.Issue) error {
	name := fmt.Sprintf("%s-%d", repo.Name, issue.Number)
	nn := types.NamespacedName{Namespace: repo.Namespace, Name: name}

	if toIssueState(issue.State) != agentswarmv1alpha1.IssueStateOpen {
		var existing agentswarmv1alpha1.Issue
		if err := r.Get(ctx, nn, &existing); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("get Issue %q for closed-state cleanup: %w", nn.String(), err)
		}
		if err := r.Delete(ctx, &existing); err != nil {
			return fmt.Errorf("delete closed Issue %q: %w", nn.String(), err)
		}
		return nil
	}

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

// pruneOutdatedIssues deletes child Issue CRs that are no longer part of the
// latest open-issue snapshot from GitHub.
func (r *RepositoryReconciler) pruneOutdatedIssues(
	ctx context.Context,
	repo *agentswarmv1alpha1.Repository,
	desiredNames map[string]struct{},
) error {
	var issueList agentswarmv1alpha1.IssueList
	if err := r.List(ctx, &issueList, client.InNamespace(repo.Namespace)); err != nil {
		return fmt.Errorf("list Issues for pruning: %w", err)
	}

	for idx := range issueList.Items {
		issue := &issueList.Items[idx]
		owner := metav1.GetControllerOf(issue)
		if owner == nil || owner.Kind != "Repository" || owner.Name != repo.Name {
			continue
		}

		if _, ok := desiredNames[issue.Name]; ok {
			continue
		}

		if err := r.Delete(ctx, issue); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete outdated Issue %q: %w", client.ObjectKeyFromObject(issue).String(), err)
		}
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
