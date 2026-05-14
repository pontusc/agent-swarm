# agent-swarm — AI Working Notes

Project-specific guidance for Claude Code. This file complements:

- `~/.claude/CLAUDE.md` — global instructions (security, agents, coding principles). Do not duplicate.
- [`operator/README.md`](operator/README.md) — architecture, trigger model, phase machine, file layout. Authoritative for "how does the operator work."
- [`operator/AGENTS.md`](operator/AGENTS.md) — kubebuilder-generated operational reference (codegen rules, scaffold markers, project layout, make-target cheat sheet). Authoritative for kubebuilder mechanics; do not duplicate here.

This file owns project-specific working guidance: collaboration mode, scope decisions, and conventions that don't naturally live in code or in the operator README.

## What this project is

A Kubernetes operator (Go, Kubebuilder) that:

1. Watches a list of GitHub repositories declared as `Repository` CRs.
2. Polls each repo's issues and materializes them as `Issue` CRs in the cluster.
3. Spawns one agent pod per `Issue` using the `opencode` CLI to attempt a fix. A separate publisher pod — the only one with GitHub App credentials — pushes a branch and opens a PR.
4. Waits for the PR to merge, then deletes the `Issue` CR (cascading the workspace and child pods/jobs).

Inspirations: ArgoCD's `Application` model (declarative pull-based sync), Tekton (one-shot pods as the unit of work).

The motivating goal: the user should be able to read any part of this codebase and understand what's happening. Pedagogical collaboration over driver autonomy.

## Collaboration mode (the important part)

Default mode is **ask, then act**.

**Before writing code:**

- Sketch the design first. Walk through what is about to be added, why this shape, what the alternatives are.
- For each new dependency, library, or pattern: name it, say what it does, justify it.
- Pause and confirm before introducing anything more than a small, obvious change.

**While writing:**

- Small reviewable steps. One CRD field, one reconciler branch, one helper — not "the whole controller".
- No silent abstractions. If introducing an interface, factory, or wrapper, surface why.
- Prefer the obvious solution to the clever one until the user asks for cleverness.

**After writing:**

- Briefly explain the new code, especially anything the user may not have seen before (kubebuilder markers, controller-runtime patterns, Go idioms).
- Point at `file:line` for every meaningful piece so the user can trace it.

**Authorization scope:**

- "Go ahead" applies to the step under discussion, not the whole feature.
- Do not run codegen (`kubebuilder create api`, `make generate`, etc.) until the user has agreed on what is being scaffolded.

Global safety rules from `~/.claude/CLAUDE.md` still apply.

## Repo layout

```
agent-swarm/
├── operator/        # kubebuilder project root (Go module). Hosts both controllers.
├── agent/           # agent container image (Dockerfile + entrypoint).
├── scripts/         # host-side helpers (minikube up/down, etc.)
├── .secrets/        # gitignored runtime secrets + committable *.example templates
├── Makefile         # top-level orchestration; wraps the operator's kubebuilder targets
└── CLAUDE.md
```

The operator binary hosts **both** `RepositoryController` (GitHub sync) and `IssueController` (agent pod lifecycle, publisher Job, PR-merge polling) — one process, one Deployment. See [`operator/README.md`](operator/README.md) for the full picture, including the credential boundary between the agent and publisher pods.

## Project conventions

- **Module path:** `github.com/pontuscurtsson/agent-swarm/operator` (operator lives in the `operator/` subdir).
- **CRD group / domain:** `agentswarm.dev`.
- **API version:** `v1alpha1`. Bump on breaking changes.
- **Kinds:** `Repository`, `Issue`. `Repository` owns `Issue`s via `ownerReferences`; deleting a `Repository` cascades.
- **Resource naming:** `Issue` CRs are named `<repository-cr-name>-<issue-number>` to keep names deterministic and reconciliation idempotent.
- **GitHub auth:** GitHub App. App ID + installation ID + private key in a Secret referenced by `Repository.spec.secretRef`. No PAT path.
- **Agent runtime:** `opencode` CLI authenticated via an opencode.ai/zen API key, mounted from a Secret. No subscription-OAuth hacks.
- **Credential boundary:** the agent pod has no GitHub token; the publisher pod has the only GitHub App credentials. See the file header of `operator/internal/controller/issue_controller_publish.go` for the design note.

## Scope (intentionally out)

These are deliberate exclusions, not TODOs:

- **No webhooks.** Polling only. Webhooks would shorten the GitHub→cluster latency but aren't required for correctness.
- **No leader election concerns.** Single-replica deployment is fine.
- **No multi-tenancy hardening.** One operator, one team-owned cluster.
- **No metrics scaffolding beyond what Kubebuilder generates.**
- **No Helm chart.** Kustomize is enough.
- **No PAT auth path.** GitHub App only.
- **No subscription-based agent auth.** Zen API key only.

## When working on this codebase

- **Verify versions before suggesting code.** `controller-runtime` and Kubebuilder APIs shift between versions. Check what is pinned in `operator/go.mod` and read the matching docs before recommending a snippet.
- **`PROJECT` is the source of truth** for which Kinds and scaffolds exist. Read it when in doubt.
- **No new tools without asking.** The toolchain is Docker + kubectl + kubebuilder + make + minikube + go. Helm/asdf/mise/sops/etc. require explicit approval.
- **Respect the scope list above.** If a request implies adding something on that list, name the mismatch and confirm before writing code.
