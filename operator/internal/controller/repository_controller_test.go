package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
	githubclient "github.com/pontuscurtsson/agent-swarm/operator/internal/github"
)

type fakeGitHubClient struct {
	issues []githubclient.Issue
	err    error
}

func (f *fakeGitHubClient) ListIssues(_ context.Context, _, _ string) ([]githubclient.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.issues, nil
}

var _ = Describe("Repository Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const secretName = "test-github-app"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		repository := &agentswarmv1alpha1.Repository{}

		BeforeEach(func() {
			By("creating the Secret with GitHub App credentials")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: "default",
				},
				StringData: map[string]string{
					"appId":          "1",
					"installationId": "2",
					"privateKey":     "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &corev1.Secret{})
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			}

			By("creating the custom resource for the Kind Repository")
			err = k8sClient.Get(ctx, typeNamespacedName, repository)
			if err != nil && errors.IsNotFound(err) {
				resource := &agentswarmv1alpha1.Repository{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: agentswarmv1alpha1.RepositorySpec{
						Owner:               "octocat",
						Repo:                "hello-world",
						SyncIntervalSeconds: 60,
						SecretRef: agentswarmv1alpha1.LocalSecretReference{
							Name: secretName,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &agentswarmv1alpha1.Repository{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				By("Cleaning up the specific resource instance Repository")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}

			secret := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, secret)
			if err == nil {
				By("Cleaning up the Secret used by Repository")
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &RepositoryReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				newGitHubClient: func(githubclient.AppCreds) (githubclient.Client, error) {
					return &fakeGitHubClient{
						issues: []githubclient.Issue{{Number: 1, Title: "Bug", State: "Open"}},
					}, nil
				},
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(60 * time.Second))

			updated := &agentswarmv1alpha1.Repository{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
			Expect(updated.Status.LastSyncTime).NotTo(BeNil())
			Expect(updated.Status.ObservedIssueCount).To(Equal(int32(1)))

			syncedCondition := meta.FindStatusCondition(updated.Status.Conditions, "Synced")
			Expect(syncedCondition).NotTo(BeNil())
			Expect(syncedCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(syncedCondition.Reason).To(Equal("SyncSucceeded"))

			issueCR := &agentswarmv1alpha1.Issue{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-1", Namespace: "default"}, issueCR)).To(Succeed())
			Expect(issueCR.Spec.Number).To(Equal(int32(1)))
			Expect(issueCR.Spec.Title).To(Equal("Bug"))
			Expect(issueCR.Spec.State).To(Equal(agentswarmv1alpha1.IssueStateOpen))

			owner := metav1.GetControllerOf(issueCR)
			Expect(owner).NotTo(BeNil())
			Expect(owner.Kind).To(Equal("Repository"))
			Expect(owner.Name).To(Equal(resourceName))
		})
	})
})
