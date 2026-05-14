# agent-swarm

Kubernetes operator that turns GitHub issues into agent-attempted pull requests.

> Status: scaffolding. See `CLAUDE.md` for the phase tracker.

## Layout

```
agent-swarm/
├── operator/        # the operator (kubebuilder project, Go module)
├── agent/           # agent container image (Phase 2)
├── scripts/         # host helpers (minikube up/down)
├── .secrets/        # gitignored secrets (+ committable *.example)
└── Makefile         # top-level orchestration
```

A single operator binary in `operator/` runs both `RepositoryController` (polls GitHub, materializes `Issue` CRs) and `IssueController` (Phase 2: per-issue agent pod).

## kubebuilder, briefly

[Kubebuilder](https://book.kubebuilder.io/) is a CLI that scaffolds Kubernetes operators. It writes the manager binary, RBAC, kustomize manifests, codegen plumbing, Dockerfile, and Makefile so we can focus on reconciliation logic.

It runs inside `operator/`. `kubebuilder init` already produced:

- `PROJECT` — metadata, source of truth (never hand-edit)
- `cmd/main.go` — controller-manager entrypoint with `// +kubebuilder:scaffold:*` markers (do not remove)
- `config/` — kustomize tree (manager, rbac, opt-in overlays for prometheus, network-policy, webhooks)
- `Makefile` — `manifests`, `generate`, `build`, `test`, `run`, `docker-build`, `deploy`, …
- `Dockerfile`, `go.mod`, `test/e2e/`, `AGENTS.md` (kubebuilder's own operational reference)

CRD types and reconcilers (`api/`, `internal/controller/`) come later — from `kubebuilder create api`, not from `init`.

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

## Quick start (once the skeleton is live)

```bash
make start-minikube                            # local cluster
kubectl apply -f .secrets/github-app.yml       # GitHub App credentials Secret
kubectl apply -f .secrets/opencode-credentials.yml  # OpenCode Zen API key Secret
cd operator/ && make install deploy            # install CRDs + deploy operator (when ready)
```

## Pending behavior notes

- Issue lifecycle policy: an `Issue` CR exists only as long as its upstream GitHub issue is open. Closed-on-GitHub issues fall out of the open-only sync snapshot and are pruned by `RepositoryController`; merge-driven completion runs the full `Done` transition (cascade-delete via ownerRefs). Either path produces the same cleanup; the merge path additionally retains the agent log ConfigMap because it isn't owned by the `Issue` CR.
- Phase 2 agent flow (draft):
  - `IssueController` detects unassigned `Issue` and allocates work.
  - Operator prepares a per-issue workspace in a PVC by cloning repo + creating branch using GitHub App credentials.
  - Agent pod mounts only the prepared workspace PVC, receives an OpenCode API key from `Secret/opencode-credentials`, runs `opencode run`, and exits with artifacts/status.
  - Current PoC prompt gives OpenCode the issue title/body and asks it to implement the requested fix with minimal focused changes.
  - Full OpenCode stdout/stderr is archived into one or more `ConfigMap` objects named `agent-log-<issue>-NNN` under `data.log.txt`.
  - Operator reads results, pushes branch, opens PR, and updates `Issue.status`.
- Issue phase state machine (current + near-term):
  - `Pending` -> `PreparingWorkspace` -> `WorkspaceReady` -> `AgentRunning` -> `PublishPending`
  - `PublishPending` -> `PRCreated` -> `Done` (when PR merge is detected)
  - `Done` triggers automatic `Issue` CR deletion; owner refs then garbage-collect workspace PVC/job/pod resources
  - any stage can transition to `Failed`

## More

- [`operator/README.md`](operator/README.md) — operator architecture, trigger flow, phase machine, file layout
- `CLAUDE.md` — project-specific working notes (phases, conventions, scope)
- `operator/AGENTS.md` — kubebuilder operational reference (codegen, markers, distribution)
- [Kubebuilder Book](https://book.kubebuilder.io/) — full upstream docs
