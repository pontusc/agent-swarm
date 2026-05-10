package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	agentswarmv1alpha1 "github.com/pontuscurtsson/agent-swarm/operator/api/v1alpha1"
	githubclient "github.com/pontuscurtsson/agent-swarm/operator/internal/github"
)

// loadCreds reads the GitHub App credentials referenced by Repository.spec.secretRef.
// We parse app/installation IDs as integers because ghinstallation expects numeric IDs,
// while Secret values are always stored as bytes.
func (r *RepositoryReconciler) loadCreds(ctx context.Context, repo *agentswarmv1alpha1.Repository) (githubclient.AppCreds, error) {
	secretName := types.NamespacedName{Namespace: repo.Namespace, Name: repo.Spec.SecretRef.Name}

	var secret corev1.Secret
	if err := r.Get(ctx, secretName, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return githubclient.AppCreds{}, fmt.Errorf("secret %q not found", secretName.String())
		}
		return githubclient.AppCreds{}, fmt.Errorf("get secret %q: %w", secretName.String(), err)
	}

	appID, err := parseRequiredInt64(secret.Data, "appId")
	if err != nil {
		return githubclient.AppCreds{}, fmt.Errorf("secret %q: %w", secretName.String(), err)
	}

	installationID, err := parseRequiredInt64(secret.Data, "installationId")
	if err != nil {
		return githubclient.AppCreds{}, fmt.Errorf("secret %q: %w", secretName.String(), err)
	}

	privateKeyPEM, ok := secret.Data["privateKey"]
	if !ok {
		return githubclient.AppCreds{}, fmt.Errorf("missing key %q", "privateKey")
	}
	if len(privateKeyPEM) == 0 {
		return githubclient.AppCreds{}, fmt.Errorf("key %q must not be empty", "privateKey")
	}

	return githubclient.AppCreds{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKeyPEM:  privateKeyPEM,
	}, nil
}

// parseRequiredInt64 validates that a required Secret key exists and contains a
// base-10 integer value. This keeps Secret shape errors explicit and user-facing.
func parseRequiredInt64(data map[string][]byte, key string) (int64, error) {
	raw, ok := data[key]
	if !ok {
		return 0, fmt.Errorf("missing key %q", key)
	}

	value := strings.TrimSpace(string(raw))
	if value == "" {
		return 0, fmt.Errorf("key %q must not be empty", key)
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q as int64: %w", key, err)
	}

	return parsed, nil
}
