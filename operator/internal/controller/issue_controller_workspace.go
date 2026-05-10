package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
)

// getOwningRepository resolves the Repository controller owner for this Issue.
func (r *IssueReconciler) getOwningRepository(ctx context.Context, issue *agentswarmv1alpha1.Issue) (*agentswarmv1alpha1.Repository, error) {
	owner := metav1.GetControllerOf(issue)
	if owner == nil || owner.Kind != "Repository" {
		return nil, fmt.Errorf("issue has no owning Repository")
	}

	var repo agentswarmv1alpha1.Repository
	key := client.ObjectKey{Namespace: issue.Namespace, Name: owner.Name}
	if err := r.Get(ctx, key, &repo); err != nil {
		return nil, fmt.Errorf("get owning Repository %q: %w", key.String(), err)
	}

	return &repo, nil
}

// ensureWorkspacePVC creates (or reuses) the per-issue workspace volume.
func (r *IssueReconciler) ensureWorkspacePVC(ctx context.Context, issue *agentswarmv1alpha1.Issue, name string) error {
	key := client.ObjectKey{Namespace: issue.Namespace, Name: name}

	var existing corev1.PersistentVolumeClaim
	if err := r.Get(ctx, key, &existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get workspace PVC %q: %w", key.String(), err)
	}

	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: issue.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(issue, &pvc, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference on PVC %q: %w", key.String(), err)
	}
	if err := r.Create(ctx, &pvc); err != nil {
		return fmt.Errorf("create workspace PVC %q: %w", key.String(), err)
	}

	return nil
}

// ensureWorkspacePrepJob creates (or reuses) the one-shot clone/checkout job.
func (r *IssueReconciler) ensureWorkspacePrepJob(
	ctx context.Context,
	issue *agentswarmv1alpha1.Issue,
	repo *agentswarmv1alpha1.Repository,
	name string,
	pvcName string,
	branch string,
) (*batchv1.Job, error) {
	key := client.ObjectKey{Namespace: issue.Namespace, Name: name}

	var existing batchv1.Job
	if err := r.Get(ctx, key, &existing); err == nil {
		return &existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get workspace prep Job %q: %w", key.String(), err)
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
							Name:            "workspace-prep",
							Image:           "alpine/git:2.47.2",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/bin/sh", "-c"},
							Args: []string{
								`set -eu
rm -rf /workspace/repo
git clone --depth=1 "https://github.com/${OWNER}/${REPO}.git" /workspace/repo
cd /workspace/repo
git checkout -B "${BRANCH}"
git symbolic-ref --short HEAD > /workspace/current-branch.txt`,
							},
							Env: []corev1.EnvVar{
								{Name: "OWNER", Value: repo.Spec.Owner},
								{Name: "REPO", Value: repo.Spec.Repo},
								{Name: "BRANCH", Value: branch},
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
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(issue, &job, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on Job %q: %w", key.String(), err)
	}
	if err := r.Create(ctx, &job); err != nil {
		return nil, fmt.Errorf("create workspace prep Job %q: %w", key.String(), err)
	}

	return &job, nil
}

// jobReachedBackoffLimit reports whether Kubernetes has exhausted retries.
func jobReachedBackoffLimit(job *batchv1.Job) bool {
	if job.Spec.BackoffLimit == nil {
		return false
	}
	return job.Status.Failed >= *job.Spec.BackoffLimit
}

// workspacePVCName keeps PVC naming deterministic and idempotent per Issue.
func workspacePVCName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("%s-workspace", issue.Name)
}

// workspacePrepJobName keeps prep Job naming deterministic and idempotent.
func workspacePrepJobName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("prep-%s", issue.Name)
}

// branchName defines the branch namespace used for agent work on an issue.
func branchName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("agent-swarm/issue-%d", issue.Spec.Number)
}
