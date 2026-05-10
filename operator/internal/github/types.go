package github

// Issue is the operator's lightweight view of a GitHub issue. It mirrors the
// fields RepositoryController materializes into v1alpha1.IssueSpec, decoupling
// the reconciler from go-github's pointer-everywhere optional types.
type Issue struct {
	Number int32
	Title  string
	Body   string
	Labels []string
	// State is "Open" or "Closed", matching v1alpha1.IssueState values.
	State string
}

// AppCreds carries the secrets needed to authenticate as a GitHub App
// installation. The reconciler reads these from the Secret referenced by
// Repository.spec.secretRef.
type AppCreds struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
}
