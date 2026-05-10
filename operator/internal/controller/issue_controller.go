package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
)

// IssueReconciler reconciles an Issue object.
type IssueReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentswarm.dev,resources=issues/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentswarm.dev,resources=repositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create

// Reconcile prepares a per-issue workspace by creating a PVC and a prep Job
// that clones the repository and checks out a dedicated branch.
func (r *IssueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var issue agentswarmv1alpha1.Issue
	if err := r.Get(ctx, req.NamespacedName, &issue); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
	const maxPrepRetries int32 = 3

	if prepJob.Status.Succeeded > 0 {
		issue.Status.Phase = agentswarmv1alpha1.IssuePhaseWorkspaceReady
		issue.Status.PrepRetries = 0
		issue.Status.LastError = ""
		meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
			Type:               "WorkspacePrepared",
			Status:             metav1.ConditionTrue,
			Reason:             "WorkspaceReady",
			Message:            "Workspace prepared and branch checked out",
			ObservedGeneration: issue.Generation,
		})
		if err := r.updateIssueStatus(ctx, &issue); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if prepJob.Status.Failed > 0 && prepJobReachedBackoffLimit(prepJob) {
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

func (r *IssueReconciler) markIssueFailed(ctx context.Context, issue *agentswarmv1alpha1.Issue, reason string, message string) error {
	issue.Status.ObservedGeneration = issue.Generation
	issue.Status.Phase = agentswarmv1alpha1.IssuePhaseFailed
	issue.Status.LastError = message
	meta.SetStatusCondition(&issue.Status.Conditions, metav1.Condition{
		Type:               "WorkspacePrepared",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: issue.Generation,
	})

	return r.updateIssueStatus(ctx, issue)
}

func (r *IssueReconciler) updateIssueStatus(ctx context.Context, issue *agentswarmv1alpha1.Issue) error {
	if err := r.Status().Update(ctx, issue); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

func prepJobReachedBackoffLimit(job *batchv1.Job) bool {
	if job.Spec.BackoffLimit == nil {
		return false
	}
	return job.Status.Failed >= *job.Spec.BackoffLimit
}

func workspacePVCName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("%s-workspace", issue.Name)
}

func workspacePrepJobName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("%s-prep", issue.Name)
}

func branchName(issue *agentswarmv1alpha1.Issue) string {
	return fmt.Sprintf("agent-swarm/issue-%d", issue.Spec.Number)
}

// SetupWithManager sets up the controller with the Manager.
func (r *IssueReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentswarmv1alpha1.Issue{}).
		Owns(&batchv1.Job{}).
		Named("issue").
		Complete(r)
}
