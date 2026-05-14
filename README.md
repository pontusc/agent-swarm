# agent-swarm

Kubernetes operator that turns GitHub issues into agent-attempted pull requests.

## Setup

Two secrets need to exist before the operator can do useful work: a **GitHub App** that authenticates the operator to GitHub (read issues, push branches, open PRs) and an **OpenCode** API key that the agent pod uses to call the model. Both are populated from local files under `.secrets/` — gitignored, with committed `*.example` templates.

### 1. GitHub App

Create a GitHub App and install it on the repositories you want the operator to watch:

1. **Settings → Developer settings → GitHub Apps → New GitHub App** (user or org).
2. **Webhooks:** leave the URL blank and uncheck _Active_. The operator polls; it does not consume webhooks.
3. **Repository permissions** (everything else can stay at _No access_):
   - **Contents:** Read & Write — push agent branches
   - **Pull requests:** Read & Write — open PRs and read merge state
   - **Issues:** Read — list the open issue set
   - **Metadata:** Read — mandatory
4. Save the App, then **Generate a private key** at the bottom of the settings page — downloads as a `.pem`.
5. **Install App** on the target repositories (account or org). After install, GitHub redirects to `…/installations/<INSTALLATION_ID>` — note that ID.
6. Note the **App ID** from the top of the App's settings page.

Populate the Secret manifest:

```bash
cp .secrets/github-app.yml.example .secrets/github-app.yml
# Edit appId, installationId, and paste the .pem contents into the
# privateKey block (preserve the leading whitespace from the example).
```

### 2. OpenCode API key

The agent pod calls the model through [opencode.ai/zen](https://opencode.ai). Create an account, generate an API key, then:

```bash
cp .secrets/opencode-credentials.yml.example .secrets/opencode-credentials.yml
# Edit and paste your API key into the apiKey field.
```

`make setup` skips the OpenCode secret apply if the file is missing and prints a reminder; `github-app.yml` is required and the deploy will fail without it.

### 3. Repositories to watch

Edit `operator/config/samples/agentswarm_v1alpha1_repository.yaml` so `spec.owner` / `spec.repo` point at a repo the GitHub App is installed on. `make setup` applies the samples automatically.

## Layout

```
agent-swarm/
├── operator/        # the operator (kubebuilder project, Go module)
├── agent/           # agent container image
├── scripts/         # host helpers (minikube up/down)
├── .secrets/        # gitignored secrets (+ committable *.example)
└── Makefile         # top-level orchestration
```

A single operator binary in `operator/` runs both `RepositoryController` (polls GitHub, materializes `Issue` CRs) and `IssueController` (per-issue agent pod lifecycle, publisher Job, PR-merge polling). See [`operator/README.md`](operator/README.md) for the full architecture.

## kubebuilder, briefly

[Kubebuilder](https://book.kubebuilder.io/) is a CLI that scaffolds Kubernetes operators. It writes the manager binary, RBAC, kustomize manifests, codegen plumbing, Dockerfile, and Makefile so we can focus on reconciliation logic.

It runs inside `operator/`. `kubebuilder init` produced:

- `PROJECT` — metadata, source of truth (never hand-edit)
- `cmd/main.go` — controller-manager entrypoint with `// +kubebuilder:scaffold:*` markers (do not remove)
- `config/` — kustomize tree (manager, rbac, opt-in overlays for prometheus, network-policy, webhooks)
- `Makefile` — `manifests`, `generate`, `build`, `run`, `docker-build`, `deploy`, …
- `Dockerfile`, `go.mod`, `AGENTS.md` (kubebuilder's own operational reference)

CRD types and reconcilers live under `api/` and `internal/controller/`, generated initially by `kubebuilder create api` and hand-edited from there.

### Extending via the CLI

Add a new Kind:

```bash
cd operator/
kubebuilder create api --group agentswarm --version v1alpha1 --kind Repository
```

This generates:

- `api/v1alpha1/repository_types.go` — the CRD's Go struct (Spec/Status). Edit this.
- `internal/controller/repository_controller.go` — reconciler skeleton. Edit this.
- Test stubs alongside both.
- Patches `cmd/main.go` and `PROJECT` at the scaffold markers.

After editing types and adding `+kubebuilder:` markers, run `make manifests generate` to regenerate the CRD YAML, RBAC, and deepcopy code from those markers.

Webhooks work the same way:

```bash
kubebuilder create webhook --group agentswarm --version v1alpha1 --kind Repository \
  --defaulting --programmatic-validation
```

For multi-version conversion add `--conversion --spoke v2`.

## Quick start

After completing [Setup](#setup):

```bash
make start-minikube     # local cluster + in-cluster registry proxy
make setup              # build + push images, apply secrets, deploy operator + samples
```

Opening an issue on any repo listed in `operator/config/samples/agentswarm_v1alpha1_repository.yaml` will trigger an agent run within `Repository.spec.syncIntervalSeconds`. Watch the lifecycle with `kubectl get issues -w`.

## Behavior notes

- **Issue lifecycle policy:** an `Issue` CR exists only as long as its upstream GitHub issue is open. Closed-on-GitHub issues fall out of the open-only sync snapshot and are pruned by `RepositoryController`; merge-driven completion runs the full `Done` transition (cascade-delete via ownerRefs). Either path produces the same cleanup; the merge path additionally retains the agent log ConfigMap because it isn't owned by the `Issue` CR.
- **Agent flow:**
  - `IssueController` ensures the per-issue workspace PVC and a prep `Job` (clone + branch checkout).
  - On prep success the agent `Pod` mounts the workspace PVC and an OpenCode API key from `Secret/opencode-credentials`, runs `opencode run` against the issue title/body, exits.
  - A separate publisher `Job` (the only pod with GitHub App credentials) commits, pushes the branch, and opens the PR.
  - Full OpenCode stdout/stderr is archived into one or more `ConfigMap` objects named `agent-log-<issue>-NNN` under `data.log.txt`. These survive `Issue` cleanup.
- **Issue phase state machine:**
  - `Pending` → `PreparingWorkspace` → `WorkspaceReady` → `AgentRunning` → `PublishPending` → `PRCreated` → `Done`
  - `Done` triggers automatic `Issue` CR deletion; owner refs garbage-collect the workspace PVC, Jobs and Pods.
  - Any stage can transition to `Failed`.

See [`operator/README.md`](operator/README.md) for the full architecture, trigger model, and Mermaid diagrams.

## More

- [`operator/README.md`](operator/README.md) — operator architecture, trigger flow, phase machine, file layout
- `CLAUDE.md` — collaboration mode, scope, conventions
- `operator/AGENTS.md` — kubebuilder operational reference
- [Kubebuilder Book](https://book.kubebuilder.io/) — upstream docs
