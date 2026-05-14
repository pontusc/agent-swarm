# operator

Kubernetes operator that turns GitHub issues into agent-attempted pull requests.

This README is for someone reading the operator code. For project-wide context (collaboration mode, scope, conventions) see [`../CLAUDE.md`](../CLAUDE.md). For kubebuilder mechanics (scaffolding markers, codegen rules) see [`AGENTS.md`](AGENTS.md).

## What it does

Two custom resources, two controllers, one binary:

- **`Repository`** declares a GitHub repo to watch. `RepositoryController` polls it every `spec.syncIntervalSeconds` and reflects the open issues as child `Issue` CRs in the cluster.
- **`Issue`** is a read-only mirror of one GitHub issue. `IssueController` drives each one through a phase machine that materializes a workspace, runs an agent against it, opens a pull request, and waits for the PR to merge before cleaning up.

Single operator binary in `cmd/main.go` hosts both controllers. Single Deployment. Polling is the only trigger from GitHub; webhook support is intentionally out of scope (see [`../CLAUDE.md`](../CLAUDE.md) → Scope).

## Architecture

```mermaid
flowchart TB
    operator([operator / human])
    gh((GitHub API))

    subgraph cluster["Kubernetes cluster"]
        direction TB

        subgraph crs["Custom Resources"]
            direction LR
            repoCR[Repository CR]
            issueCR[Issue CR]
            repoCR -. owns .-> issueCR
        end

        subgraph mgr["agent-swarm-controller-manager (one Deployment)"]
            direction LR
            repoCtrl[RepositoryController]
            issueCtrl[IssueController]
        end

        subgraph pipeline["Per-issue pipeline (shared workspace PVC)"]
            direction LR
            prep[1. prep Job<br/>clone + checkout]
            agent[2. agent Pod<br/>opencode run]
            publish[3. publish Job<br/>commit + push + PR]
            prep --> agent --> publish
        end

        cm[(agent-log ConfigMap<br/>survives Issue cleanup)]
    end

    operator -- declares --> repoCR
    repoCtrl -- watches --> repoCR
    repoCtrl -- polls every N s --> gh
    repoCtrl -- creates/prunes --> issueCR

    issueCtrl -- watches --> issueCR
    issueCtrl -- drives --> pipeline
    issueCtrl -- archives logs --> cm
    issueCtrl -- polls PR merge --> gh

    publish -- push branch + open PR --> gh
```

Read top to bottom: a human declares a `Repository`, `RepositoryController` polls GitHub and creates child `Issue`s, `IssueController` drives each `Issue` through a three-stage pipeline that shares one PVC, and the publish stage is the only one with GitHub credentials.

### Credential boundary

The agent Pod and the publisher Pod are deliberately separate processes with different credentials:

| Pod               | Has                                              | Can do                                        |
| ----------------- | ------------------------------------------------ | --------------------------------------------- |
| `agent-<issue>`   | OpenCode API key + workspace volume              | Run LLM, write files to workspace             |
| `publish-<issue>` | GitHub App `appId`/`installationId`/`privateKey` | Sign installation token, push branch, open PR |

The agent has no GitHub token. If the LLM goes off-script the workspace gets dirty, but nothing leaves the cluster. The publisher is a fixed bash script with no agent-supplied inputs (only operator-supplied env vars) — its only input from the agent is "whatever files are in the workspace." Humans review the PR before merge.

See the file header of [`internal/controller/issue_controller_publish.go`](internal/controller/issue_controller_publish.go) for the full design note.

## Trigger model

Three distinct trigger types drive the operator. The sequence diagram below is divided into three `Note over` sections, one per trigger.

1. **GitHub-poll trigger** — `RepositoryController` requeues itself every `Repository.spec.syncIntervalSeconds`, lists GitHub's open issues, and writes the diff into the cluster as `Issue` CRs.
2. **Cluster-watch trigger** — `IssueController.SetupWithManager` calls `Owns(Job, Pod)`. Job/Pod status changes wake the reconciler through the K8s watch stream — the workspace pipeline advances reactively, not by polling.
3. **PR-poll trigger** — once an Issue lands in `PRCreated`, the reconciler requeues every 30s and asks GitHub whether the PR has merged.

The handoff between triggers is K8s state: trigger 1 ends by creating an `Issue` CR (which trigger 2 watches), trigger 2 ends by writing `status.prUrl` + `phase=PRCreated` (which trigger 3 reacts to).

### End-to-end sequence

```mermaid
sequenceDiagram
    actor Human
    participant GH as GitHub
    participant Repo as RepositoryReconciler
    participant K8s as K8s API
    participant Issue as IssueReconciler
    participant Prep as prep Job
    participant Agent as agent Pod
    participant Pub as publish Job

    Note over Repo,K8s: Trigger 1 — GitHub poll (every syncIntervalSeconds)
    Human->>GH: open issue #N
    Repo->>GH: GET /issues?state=open
    GH-->>Repo: [...]
    Repo->>K8s: create Issue CR (<repo>-N)

    Note over K8s,Pub: Trigger 2 — cluster watch (Owns(Job)+Owns(Pod) wakes IssueReconciler)
    K8s-->>Issue: Issue CR ADDED
    Issue->>K8s: create PVC + prep Job
    K8s-->>Prep: schedule Pod
    Prep->>Prep: git clone + checkout
    Prep-->>K8s: Pod Succeeded
    K8s-->>Issue: prep Pod transition
    Issue->>K8s: create agent Pod
    K8s-->>Agent: schedule Pod
    Agent->>Agent: opencode run
    Agent-->>K8s: Pod Succeeded
    K8s-->>Issue: agent Pod transition
    Issue->>K8s: create publish Job
    K8s-->>Pub: schedule Pod
    Pub->>GH: JWT → installation token
    Pub->>GH: git push branch
    Pub->>GH: POST /pulls
    GH-->>Pub: pr.html_url (→ terminationMessage)
    Pub-->>K8s: Pod Succeeded
    K8s-->>Issue: publish Pod transition
    Issue->>K8s: status.prUrl = …, phase = PRCreated

    Note over Issue,GH: Trigger 3 — PR poll (every 30s while PRCreated)
    Issue->>GH: GET /pulls/M
    GH-->>Issue: open, not merged
    Human->>GH: merge PR
    Issue->>GH: GET /pulls/M
    GH-->>Issue: merged = true
    Issue->>K8s: phase = Done, delete Issue CR
    K8s->>K8s: ownerRefs cascade → PVC, Jobs, Pods deleted
```

`K8s` here is the apiserver acting as the message bus — `create` calls fire watch events that other reconcilers receive, which is how trigger 1 hands off to trigger 2 without either reconciler knowing about the other directly.

### Phase machine

The `Issue` CR walks a fixed set of phases. Any phase can land in `Failed`; terminal success is `Done`.

```mermaid
stateDiagram-v2
    [*] --> Pending: Issue CR created
    Pending --> PreparingWorkspace: ensure PVC + prep Job
    PreparingWorkspace --> WorkspaceReady: prep Job Succeeded
    PreparingWorkspace --> PreparingWorkspace: retry (bounded)
    WorkspaceReady --> AgentRunning: ensure agent Pod
    AgentRunning --> PublishPending: agent Pod Succeeded
    PublishPending --> PRCreated: publish Job Succeeded
    PRCreated --> Done: PR merged
    Done --> [*]: cascade delete

    PreparingWorkspace --> Failed: retries exhausted
    AgentRunning --> Failed: agent Pod Failed
    PublishPending --> Failed: publish Job Failed
    PRCreated --> Failed: PR closed without merge
```

The phase ↔ handler map lives at the top of [`internal/controller/issue_controller.go`](internal/controller/issue_controller.go). Each phase is owned by a single function in a single file; the dispatcher in `Reconcile` is the only place that decides which one runs.

## File layout

```
operator/
├── cmd/main.go                       # manager binary entrypoint, env var resolution
├── api/v1alpha1/                     # CRD Go types (Repository, Issue)
├── internal/
│   ├── controller/                   # reconcilers
│   │   ├── repository_controller.go             # poll loop
│   │   ├── repository_controller_issue_sync.go  # Issue CR diffing
│   │   ├── repository_controller_status.go      # Synced=False writer
│   │   ├── issue_controller.go                  # phase dispatcher + workspace prep
│   │   ├── issue_controller_workspace.go        # PVC + prep Job builders
│   │   ├── issue_controller_agent.go            # agent Pod + log archival
│   │   ├── issue_controller_publish.go          # publisher Job (credential boundary)
│   │   ├── issue_controller_pr.go               # PR-merge polling
│   │   ├── issue_controller_status.go           # status write helpers
│   │   └── credentials.go                       # GitHub App secret loader (shared)
│   └── github/                       # narrow GitHub adapter (ghinstallation + go-github)
├── config/                           # kustomize tree (manager, RBAC, CRDs, samples)
├── Dockerfile                        # distroless static, non-root
├── PROJECT                           # kubebuilder source of truth (do not hand-edit)
└── AGENTS.md                         # kubebuilder operational reference
```

## Configuration knobs

| Knob                                  | Where                                              | Purpose                                                                                                                                                   |
| ------------------------------------- | -------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `AGENT_IMAGE`                         | env on manager Pod (`config/manager/manager.yaml`) | Override the per-issue agent Pod image. Falls back to `controller.DefaultAgentImage` (minikube path) when unset.                                          |
| `Repository.spec.syncIntervalSeconds` | CR                                                 | How often `RepositoryController` polls GitHub. Minimum 30s.                                                                                               |
| `Repository.spec.secretRef.name`      | CR                                                 | Name of the Secret carrying GitHub App credentials. Same Secret is read by both `RepositoryController` (issue listing) and the publisher Pod (push + PR). |

## Development

Run from `operator/` for code-level iteration:

| Command                           | What it does                                                   |
| --------------------------------- | -------------------------------------------------------------- |
| `make manifests`                  | Regenerate CRD YAML and RBAC from `+kubebuilder:` markers      |
| `make generate`                   | Regenerate `zz_generated.deepcopy.go`                          |
| `make fmt vet`                    | Format + static check                                          |
| `make lint` / `make lint-fix`     | golangci-lint (config at `.golangci.yml`)                      |
| `make build`                      | Build the manager binary to `bin/manager`                      |
| `make run`                        | Run the manager directly against the active kubeconfig         |
| `make docker-build IMG=...`       | Build the manager container image                              |
| `make install` / `make uninstall` | Apply or remove CRDs                                           |
| `make deploy` / `make undeploy`   | Apply or remove the full operator (kustomize `config/default`) |

The top-level [`../Makefile`](../Makefile) wraps these with the right `IMG=` values for the in-tree minikube setup (`make redeploy`, `make setup`, `make cluster-clean`). Use the wrapper for routine deploy churn; the operator-local `make` is what you reach for when iterating without redeploying.

## See also

- [`../CLAUDE.md`](../CLAUDE.md) — phase tracker, collaboration mode, scope guardrails
- [`AGENTS.md`](AGENTS.md) — kubebuilder operational reference (codegen, markers, distribution)
- [`../README.md`](../README.md) — top-level repo orientation
- [Kubebuilder Book](https://book.kubebuilder.io/) — upstream docs
