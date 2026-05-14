package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
)

const (
	opencodeAgentImage            = "localhost:5000/agent-swarm/agent:latest"
	opencodeCredentialsSecretName = "opencode-credentials"
	opencodeCredentialsSecretKey  = "apiKey"
	defaultOpenCodeModel          = "opencode/gpt-5.4-mini"
)

// reconcileAgent assigns an agent pod once workspace prep is complete.
// It tracks pod lifecycle and advances issue phase toward publish handoff.
func (r *IssueReconciler) reconcileAgent(
	ctx context.Context,
	issue *agentswarmv1alpha1.Issue,
	repo *agentswarmv1alpha1.Repository,
	workspacePVC string,
) (ctrl.Result, error) {
	pod, err := r.ensureAgentPod(ctx, issue, workspacePVC)
	if err != nil {
		return ctrl.Result{}, err
	}

	issue.Status.PrepRetries = 0
	issue.Status.AgentPodName = pod.Name
	issue.Status.LastError = ""

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		issue.Status.Phase = agentswarmv1alpha1.IssuePhasePublishPending
		issue.Status.PublishJobName = publishJobName(issue)
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "WorkspacePrepared",
			Status:             metav1.ConditionTrue,
			Reason:             "WorkspaceReady",
			Message:            "Workspace prepared and branch checked out",
			ObservedGeneration: issue.Generation,
		})
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "AgentCompleted",
			Status:             metav1.ConditionTrue,
			Reason:             "AgentRunSucceeded",
			Message:            "Agent pod completed successfully",
			ObservedGeneration: issue.Generation,
		})
		return r.reconcilePublish(ctx, issue, repo, workspacePVC)
	case corev1.PodFailed:
		message := fmt.Sprintf("agent pod %q failed", pod.Name)
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
		issue.Status.LastError = message
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "AgentCompleted",
			Status:             metav1.ConditionFalse,
			Reason:             "AgentRunFailed",
			Message:            message,
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	default:
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseAgentRunning
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "WorkspacePrepared",
			Status:             metav1.ConditionTrue,
			Reason:             "WorkspaceReady",
			Message:            "Workspace prepared and branch checked out",
			ObservedGeneration: issue.Generation,
		})
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "AgentCompleted",
			Status:             metav1.ConditionFalse,
			Reason:             "AgentRunning",
			Message:            "Agent pod is running",
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// ensureAgentPod creates (or reuses) the agent pod that mounts the prepared
// workspace PVC and runs OpenCode against the checked out repository.
func (r *IssueReconciler) ensureAgentPod(ctx context.Context, issue *agentswarmv1alpha1.Issue, workspacePVC string) (*corev1.Pod, error) {
	name := agentPodName(issue)
	key := client.ObjectKey{Namespace: issue.Namespace, Name: name}

	var existing corev1.Pod
	if err := r.Get(ctx, key, &existing); err == nil {
		return &existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get agent pod %q: %w", key.String(), err)
	}

	labelsValue := strings.Join(issue.Spec.Labels, ",")

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: issue.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:            "agent",
					Image:           opencodeAgentImage,
					ImagePullPolicy: corev1.PullAlways,
					Env: []corev1.EnvVar{
						{Name: "AGENT_WORKSPACE", Value: "/workspace/repo"},
						{Name: "ISSUE_NUMBER", Value: fmt.Sprintf("%d", issue.Spec.Number)},
						{Name: "ISSUE_TITLE", Value: issue.Spec.Title},
						{Name: "ISSUE_BODY", Value: issue.Spec.Body},
						{Name: "ISSUE_LABELS", Value: labelsValue},
						{Name: "OPENCODE_MODEL", Value: defaultOpenCodeModel},
						{
							Name: "OPENCODE_API_KEY",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: opencodeCredentialsSecretName},
								Key:                  opencodeCredentialsSecretKey,
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
	}

	if err := controllerutil.SetControllerReference(issue, &pod, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on agent pod %q: %w", key.String(), err)
	}
	if err := r.Create(ctx, &pod); err != nil {
		return nil, fmt.Errorf("create agent pod %q: %w", key.String(), err)
	}

	return &pod, nil
}

// agentPodName keeps agent pod naming deterministic per issue.
func agentPodName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("agent-%s", issue.Name)
}
