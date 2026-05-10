package controller

import (
	"context"
	"fmt"
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

// reconcileMockAgent assigns a mock agent pod once workspace prep is complete.
// It tracks pod lifecycle and advances issue phase toward publish handoff.
func (r *IssueReconciler) reconcileMockAgent(ctx context.Context, issue *agentswarmv1alpha1.Issue, workspacePVC string) (ctrl.Result, error) {
	pod, err := r.ensureMockAgentPod(ctx, issue, workspacePVC)
	if err != nil {
		return ctrl.Result{}, err
	}

	issue.Status.PrepRetries = 0
	issue.Status.AgentPodName = pod.Name
	issue.Status.LastError = ""

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		issue.Status.Phase = agentswarmv1alpha1.IssuePhasePublishPending
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
			Message:            "Mock agent pod completed successfully",
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	case corev1.PodFailed:
		message := fmt.Sprintf("mock agent pod %q failed", pod.Name)
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
			Message:            "Mock agent pod is running",
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// ensureMockAgentPod creates (or reuses) the mock agent pod that mounts
// the prepared workspace PVC and writes execution artifacts.
func (r *IssueReconciler) ensureMockAgentPod(ctx context.Context, issue *agentswarmv1alpha1.Issue, workspacePVC string) (*corev1.Pod, error) {
	name := mockAgentPodName(issue)
	key := client.ObjectKey{Namespace: issue.Namespace, Name: name}

	var existing corev1.Pod
	if err := r.Get(ctx, key, &existing); err == nil {
		return &existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get mock agent pod %q: %w", key.String(), err)
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: issue.Namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:            "agent-mock",
					Image:           "alpine/git:2.47.2",
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"/bin/sh", "-c"},
					Args: []string{
						`set -eu
cd /workspace/repo
git symbolic-ref --short HEAD > /workspace/agent-observed-branch.txt
printf 'mock-agent-ran\n' > /workspace/agent-result.txt
printf 'mock agent touched workspace\n' >> /workspace/repo/.agent-mock-output`,
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
		return nil, fmt.Errorf("set owner reference on mock agent pod %q: %w", key.String(), err)
	}
	if err := r.Create(ctx, &pod); err != nil {
		return nil, fmt.Errorf("create mock agent pod %q: %w", key.String(), err)
	}

	return &pod, nil
}

// mockAgentPodName keeps mock agent pod naming deterministic per issue.
func mockAgentPodName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("agent-mock-%s", issue.Name)
}
