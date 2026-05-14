package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	githubclient "github.com/pontuscurtsson/agent-swarm/operator/internal/github"
)

// loadGitHubAppCreds reads GitHub App credentials from a Secret. Shared by
// RepositoryReconciler (for issue polling) and IssueReconciler (for PR
// merge-state checks) because both authenticate to GitHub as the same App.
//
// Expected Secret shape (matches .secrets/github-app.yml.example):
//
//	appId:          base-10 integer, ghinstallation requires int64
//	installationId: base-10 integer, ghinstallation requires int64
//	privateKey:     PEM-encoded RSA private key bytes
//
// Secret values are always stored as []byte, so the numeric fields are
// stringified-then-parsed. Shape errors are returned with the secret name
// quoted so they surface cleanly in status messages.
func loadGitHubAppCreds(ctx context.Context, c client.Client, namespace, secretName string) (githubclient.AppCreds, error) {
	key := types.NamespacedName{Namespace: namespace, Name: secretName}

	var secret corev1.Secret
	if err := c.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return githubclient.AppCreds{}, fmt.Errorf("secret %q not found", key.String())
		}
		return githubclient.AppCreds{}, fmt.Errorf("get secret %q: %w", key.String(), err)
	}

	appID, err := parseRequiredInt64(secret.Data, "appId")
	if err != nil {
		return githubclient.AppCreds{}, fmt.Errorf("secret %q: %w", key.String(), err)
	}

	installationID, err := parseRequiredInt64(secret.Data, "installationId")
	if err != nil {
		return githubclient.AppCreds{}, fmt.Errorf("secret %q: %w", key.String(), err)
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

// parseRequiredInt64 reads a base-10 integer from a Secret data map. Returns
// a user-facing error if the key is absent, empty, or non-numeric — these
// errors land in Repository.status.conditions for operator-visible debugging.
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
