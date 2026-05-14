// Agent-phase helpers: the per-issue agent Pod that runs OpenCode against
// the prepared workspace, and the post-run log archival into ConfigMaps so
// logs outlive the Pod (and the Issue CR itself on Done cleanup).
//
// The agent Pod is intentionally credential-starved: it receives only an
// OpenCode API key and the workspace volume. GitHub App credentials live
// exclusively in the publisher Pod (issue_controller_publish.go) which
// runs after the agent has exited.
package controller

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
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
	opencodeCredentialsSecretName = "opencode-credentials"
	opencodeCredentialsSecretKey  = "apiKey"
	defaultOpenCodeModel          = "opencode/gpt-5.4-mini"
	agentLogChunkBytes            = 768 * 1024
)

// DefaultAgentImage is the fallback image when the AGENT_IMAGE env var is
// unset at operator startup. The literal points at the minikube in-cluster
// registry (see scripts/minikube/start-minikube.sh + the host-side proxy).
// Non-minikube deployments override AGENT_IMAGE in config/manager/manager.yaml.
const DefaultAgentImage = "localhost:5000/agent-swarm/agent:latest"

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// reconcileAgent ensures the agent pod for an Issue exists and advances the
// Issue's phase based on the pod's lifecycle. The pod-running branch is
// invoked every 10s while the agent works; status writes there are gated by
// `changed` so we don't generate API churn on every wakeup. The terminal
// branches (succeeded/failed) always write because they represent a real
// state transition.
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

	// PrepRetries / AgentPodName / LastError are scalar status fields we want
	// to reflect the current run. Track whether any actually changed so the
	// pod-running branch can skip a redundant Status().Update().
	changed := setIfChanged(&issue.Status.PrepRetries, 0)
	changed = setIfChanged(&issue.Status.AgentPodName, pod.Name) || changed
	changed = setIfChanged(&issue.Status.LastError, "") || changed

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		if err := r.persistAgentLogConfigMaps(ctx, issue, pod.Name); err != nil {
			return ctrl.Result{}, err
		}

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
		if err := r.persistAgentLogConfigMaps(ctx, issue, pod.Name); err != nil {
			return ctrl.Result{}, err
		}

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
		changed = setIfChanged(&issue.Status.Phase, agentswarmv1alpha1.IssuePhaseAgentRunning) || changed
		changed = meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "WorkspacePrepared",
			Status:             metav1.ConditionTrue,
			Reason:             "WorkspaceReady",
			Message:            "Workspace prepared and branch checked out",
			ObservedGeneration: issue.Generation,
		}) || changed
		changed = meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "AgentCompleted",
			Status:             metav1.ConditionFalse,
			Reason:             "AgentRunning",
			Message:            "Agent pod is running",
			ObservedGeneration: issue.Generation,
		}) || changed
		if changed {
			if err := r.updateIssueStatus(ctx, issue); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// setIfChanged writes value to *field and returns true iff the previous value
// differed. Used to gate status writes — the apiserver would no-op identical
// updates, but the round-trip itself is what we're avoiding.
func setIfChanged[T comparable](field *T, value T) bool {
	if *field == value {
		return false
	}
	*field = value
	return true
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
					Image:           r.AgentImage,
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

// persistAgentLogConfigMaps stores full agent pod logs in one or more
// deterministic ConfigMaps so logs remain available after Issue cleanup.
func (r *IssueReconciler) persistAgentLogConfigMaps(ctx context.Context, issue *agentswarmv1alpha1.Issue, podName string) error {
	if r.KubeClient == nil {
		return fmt.Errorf("kubernetes client is not configured")
	}

	logOutput, err := r.readPodLogs(ctx, issue.Namespace, podName)
	if err != nil {
		return err
	}
	logOutput = sanitizeAgentLog(logOutput)
	if logOutput == "" {
		logOutput = "<empty agent log output>\n"
	}

	parts := splitLogOutput(logOutput, agentLogChunkBytes)
	baseName := agentLogConfigMapBaseName(issue)

	for idx, part := range parts {
		name := fmt.Sprintf("%s-%03d", baseName, idx)
		if err := r.upsertAgentLogConfigMap(ctx, issue, name, podName, idx, len(parts), part); err != nil {
			return err
		}
	}

	return nil
}

// readPodLogs streams the agent pod's stdout/stderr and returns it as a string.
// Named return so the deferred Close can surface a close error when the read
// itself succeeded — otherwise the read error wins and the close error is
// suppressed (it would just be noise on a body we already consumed).
func (r *IssueReconciler) readPodLogs(ctx context.Context, namespace, podName string) (out string, retErr error) {
	stream, err := r.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs for pod %q/%q: %w", namespace, podName, err)
	}
	defer func() {
		if cerr := stream.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("close log stream for %q/%q: %w", namespace, podName, cerr)
		}
	}()

	logBytes, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("read logs for pod %q/%q: %w", namespace, podName, err)
	}

	return string(logBytes), nil
}

func (r *IssueReconciler) upsertAgentLogConfigMap(
	ctx context.Context,
	issue *agentswarmv1alpha1.Issue,
	name string,
	podName string,
	partNumber int,
	totalParts int,
	logChunk string,
) error {
	key := client.ObjectKey{Namespace: issue.Namespace, Name: name}

	labels := map[string]string{
		"agentswarm.dev/agent-log":  "true",
		"agentswarm.dev/issue-name": issue.Name,
	}
	annotations := map[string]string{
		"agentswarm.dev/issue-number": fmt.Sprintf("%d", issue.Spec.Number),
		"agentswarm.dev/log-part":     fmt.Sprintf("%d", partNumber),
		"agentswarm.dev/log-parts":    fmt.Sprintf("%d", totalParts),
		"agentswarm.dev/pod-name":     podName,
	}

	var existing corev1.ConfigMap
	if err := r.Get(ctx, key, &existing); err == nil {
		existing.Labels = labels
		existing.Annotations = annotations
		existing.Data = map[string]string{"log.txt": logChunk}
		if err := r.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update agent log ConfigMap %q: %w", key.String(), err)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get agent log ConfigMap %q: %w", key.String(), err)
	}

	configMap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   issue.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Data: map[string]string{"log.txt": logChunk},
	}

	if err := r.Create(ctx, &configMap); err != nil {
		return fmt.Errorf("create agent log ConfigMap %q: %w", key.String(), err)
	}

	return nil
}

func splitLogOutput(output string, chunkSize int) []string {
	if chunkSize <= 0 {
		return []string{output}
	}

	if len(output) <= chunkSize {
		return []string{output}
	}

	parts := make([]string, 0, (len(output)/chunkSize)+1)
	for start := 0; start < len(output); start += chunkSize {
		end := min(start+chunkSize, len(output))
		parts = append(parts, output[start:end])
	}

	return parts
}

func agentLogConfigMapBaseName(issue *agentswarmv1alpha1.Issue) string {
	base := fmt.Sprintf("agent-log-%s", issue.Name)
	maxLength := 59
	if len(base) <= maxLength {
		return base
	}

	hash := sha1.Sum([]byte(base))
	suffix := hex.EncodeToString(hash[:4])
	prefixLength := max(maxLength-len(suffix)-1, 1)

	return fmt.Sprintf("%s-%s", base[:prefixLength], suffix)
}

func sanitizeAgentLog(logOutput string) string {
	if logOutput == "" {
		return ""
	}

	sanitized := ansiEscapePattern.ReplaceAllString(logOutput, "")
	sanitized = strings.ReplaceAll(sanitized, "\r", "")

	return sanitized
}
