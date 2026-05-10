package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
)

// reconcilePublish runs after mock agent completion and pushes mock output to
// the issue branch using a short-lived publisher job with GitHub App creds.
func (r *IssueReconciler) reconcilePublish(
	ctx context.Context,
	issue *agentswarmv1alpha1.Issue,
	repo *agentswarmv1alpha1.Repository,
	workspacePVC string,
) (ctrl.Result, error) {
	jobName := publishJobName(issue)
	issue.Status.PublishJobName = jobName

	job, err := r.ensurePublishJob(ctx, issue, repo, workspacePVC, jobName)
	if err != nil {
		return ctrl.Result{}, err
	}

	meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "AgentCompleted",
		Status:             metav1.ConditionTrue,
		Reason:             "AgentRunSucceeded",
		Message:            "Mock agent pod completed successfully",
		ObservedGeneration: issue.Generation,
	})

	if job.Status.Succeeded > 0 {
		prURL, err := r.readJobTerminationMessage(ctx, issue.Namespace, jobName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if prURL == "" {
			message := fmt.Sprintf("publish job %q succeeded but did not report PR URL", jobName)
			issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
			issue.Status.LastError = message
			meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
				Type:               "Published",
				Status:             metav1.ConditionFalse,
				Reason:             "PublishFailed",
				Message:            message,
				ObservedGeneration: issue.Generation,
			})
			if err := r.updateIssueStatus(ctx, issue); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		issue.Status.Phase = agentswarmv1alpha1.IssuePhasePRCreated
		issue.Status.PRURL = prURL
		issue.Status.LastError = ""
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "Published",
			Status:             metav1.ConditionTrue,
			Reason:             "PublishSucceeded",
			Message:            "Mock agent output pushed to issue branch",
			ObservedGeneration: issue.Generation,
		})
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "PullRequestCreated",
			Status:             metav1.ConditionTrue,
			Reason:             "PullRequestCreated",
			Message:            fmt.Sprintf("Pull request created: %s", prURL),
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if job.Status.Failed > 0 && jobReachedBackoffLimit(job) {
		message := fmt.Sprintf("publish job %q failed", jobName)
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
		issue.Status.LastError = message
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "Published",
			Status:             metav1.ConditionFalse,
			Reason:             "PublishFailed",
			Message:            message,
			ObservedGeneration: issue.Generation,
		})
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "PullRequestCreated",
			Status:             metav1.ConditionFalse,
			Reason:             "PullRequestNotCreated",
			Message:            message,
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	issue.Status.Phase = agentswarmv1alpha1.IssuePhasePublishPending
	issue.Status.PRURL = ""
	issue.Status.LastError = ""
	meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "Published",
		Status:             metav1.ConditionFalse,
		Reason:             "Publishing",
		Message:            "Publishing mock agent output to GitHub branch",
		ObservedGeneration: issue.Generation,
	})
	meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "PullRequestCreated",
		Status:             metav1.ConditionFalse,
		Reason:             "PullRequestPending",
		Message:            "Waiting for publish job to create pull request",
		ObservedGeneration: issue.Generation,
	})
	if err := r.updateIssueStatus(ctx, issue); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// ensurePublishJob creates (or reuses) the one-shot job that commits, pushes,
// and opens or reuses a pull request for the issue branch.
func (r *IssueReconciler) ensurePublishJob(
	ctx context.Context,
	issue *agentswarmv1alpha1.Issue,
	repo *agentswarmv1alpha1.Repository,
	workspacePVC string,
	name string,
) (*batchv1.Job, error) {
	key := client.ObjectKey{Namespace: issue.Namespace, Name: name}

	var existing batchv1.Job
	if err := r.Get(ctx, key, &existing); err == nil {
		return &existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get publish Job %q: %w", key.String(), err)
	}

	backoffLimit := int32(1)
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: issue.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "publisher",
							Image:           "alpine:3.22",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/bin/sh", "-c"},
							Args: []string{
								`set -eu
apk add --no-cache git curl jq openssl

cd /workspace/repo
test -f .agent-mock-output

git config user.name "agent-swarm-bot"
git config user.email "agent-swarm@local"
git add .agent-mock-output

if git diff --cached --quiet; then
  echo "No changes to publish"
else
  git commit -m "mock agent output for issue ${ISSUE_NUMBER}"
fi

KEY_FILE=/tmp/github-app.pem
printf '%s' "$GITHUB_APP_PRIVATE_KEY" > "$KEY_FILE"

b64url() {
  openssl base64 -A | tr '+/' '-_' | tr -d '='
}

NOW=$(date +%s)
IAT=$((NOW - 60))
EXP=$((NOW + 540))

HEADER='{"alg":"RS256","typ":"JWT"}'
PAYLOAD=$(printf '{"iat":%s,"exp":%s,"iss":"%s"}' "$IAT" "$EXP" "$GITHUB_APP_ID")
HEADER_B64=$(printf '%s' "$HEADER" | b64url)
PAYLOAD_B64=$(printf '%s' "$PAYLOAD" | b64url)
UNSIGNED="${HEADER_B64}.${PAYLOAD_B64}"
SIGNATURE=$(printf '%s' "$UNSIGNED" | openssl dgst -binary -sha256 -sign "$KEY_FILE" | b64url)
JWT="${UNSIGNED}.${SIGNATURE}"

TOKEN_RESPONSE=$(curl -sS -X POST \
  -H "Authorization: Bearer ${JWT}" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/app/installations/${GITHUB_INSTALLATION_ID}/access_tokens")

TOKEN=$(printf '%s' "$TOKEN_RESPONSE" | jq -r '.token')
if [ -z "$TOKEN" ] || [ "$TOKEN" = "null" ]; then
  echo "Failed to get installation token: $TOKEN_RESPONSE" >&2
  exit 1
fi

git push "https://x-access-token:${TOKEN}@github.com/${OWNER}/${REPO}.git" "${BRANCH}:${BRANCH}"
printf '%s\n' "$BRANCH" > /workspace/published-branch.txt

EXISTING=$(curl -sS --get \
  -H "Authorization: token ${TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  --data-urlencode "state=open" \
  --data-urlencode "head=${OWNER}:${BRANCH}" \
  "https://api.github.com/repos/${OWNER}/${REPO}/pulls")

PR_URL=$(printf '%s' "$EXISTING" | jq -r 'if type=="array" and length>0 then .[0].html_url else empty end')

if [ -z "$PR_URL" ]; then
  REPO_RESPONSE=$(curl -sS \
    -H "Authorization: token ${TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${OWNER}/${REPO}")
  BASE_BRANCH=$(printf '%s' "$REPO_RESPONSE" | jq -r '.default_branch')

  CREATE_PAYLOAD=$(jq -n \
    --arg title "agent-swarm: issue #${ISSUE_NUMBER}" \
    --arg head "$BRANCH" \
    --arg base "$BASE_BRANCH" \
    --arg body "Automated mock-agent PR for issue #${ISSUE_NUMBER}

Closes #${ISSUE_NUMBER}" \
    '{title:$title, head:$head, base:$base, body:$body, maintainer_can_modify:true}')

  CREATED=$(curl -sS -X POST \
    -H "Authorization: token ${TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    -H "Content-Type: application/json" \
    -d "$CREATE_PAYLOAD" \
    "https://api.github.com/repos/${OWNER}/${REPO}/pulls")

  PR_URL=$(printf '%s' "$CREATED" | jq -r '.html_url // empty')
fi

if [ -z "$PR_URL" ]; then
  echo "Failed to determine/create PR URL" >&2
  exit 1
fi

printf '%s\n' "$PR_URL" > /workspace/pull-request-url.txt
printf '%s\n' "$PR_URL" > /dev/termination-log`,
							},
							Env: []corev1.EnvVar{
								{Name: "OWNER", Value: repo.Spec.Owner},
								{Name: "REPO", Value: repo.Spec.Repo},
								{Name: "BRANCH", Value: issue.Status.Branch},
								{Name: "ISSUE_NUMBER", Value: fmt.Sprintf("%d", issue.Spec.Number)},
								{
									Name: "GITHUB_APP_ID",
									ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: repo.Spec.SecretRef.Name},
										Key:                  "appId",
									}},
								},
								{
									Name: "GITHUB_INSTALLATION_ID",
									ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: repo.Spec.SecretRef.Name},
										Key:                  "installationId",
									}},
								},
								{
									Name: "GITHUB_APP_PRIVATE_KEY",
									ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
										LocalObjectReference: corev1.LocalObjectReference{Name: repo.Spec.SecretRef.Name},
										Key:                  "privateKey",
									}},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspacePVC},
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(issue, &job, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on publish job %q: %w", key.String(), err)
	}
	if err := r.Create(ctx, &job); err != nil {
		return nil, fmt.Errorf("create publish job %q: %w", key.String(), err)
	}

	return &job, nil
}

// readJobTerminationMessage returns the terminated container message from the
// succeeded pod created by the given job.
func (r *IssueReconciler) readJobTerminationMessage(ctx context.Context, namespace, jobName string) (string, error) {
	var pods corev1.PodList
	if err := r.List(
		ctx,
		&pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"job-name": jobName},
	); err != nil {
		return "", fmt.Errorf("list pods for job %q: %w", jobName, err)
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodSucceeded {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Terminated == nil {
				continue
			}
			message := strings.TrimSpace(status.State.Terminated.Message)
			if message != "" {
				return message, nil
			}
		}
	}

	return "", nil
}

// publishJobName keeps publisher Job naming deterministic and role-first.
func publishJobName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("publish-%s", issue.Name)
}
