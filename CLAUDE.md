# agent-swarm — AI Working Notes

Project-specific guidance for Claude Code. This file complements `~/.claude/CLAUDE.md` (global) — do not duplicate.

This project is a **restart of `~/Work/chas-exam`**. The previous attempt was driven mostly by Claude with the user along for the ride; by the end the user did not know how the codebase fit together. The goal of this restart is for the user to understand every part of the code.

## What this project is

A Kubernetes operator (Go, Kubebuilder) that:

1. Watches a list of GitHub repositories declared as `Repository` CRs.
2. Polls each repo's issues and materializes them as `Issue` CRs in the cluster.
3. (Phase 2) Spawns one agent pod per `Issue` using the `opencode` CLI to attempt a fix, push a branch, and open a PR via a GitHub App.

Inspirations: ArgoCD's `Application` model (declarative pull-based sync), Tekton (one-shot pods as the unit of work).

## Collaboration mode (the important part)

The user wants **pedagogical collaboration**, not driver autonomy. Default mode is **ask, then act** — the inverse of the previous attempt.

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

## Phase tracker

- [~] **Phase 0 — Skeleton.** Minikube setup, GitHub App secret, gitignore. _(In progress — see "Already in place" below.)_
- [ ] **Phase 1 — Sync.** GitHub issues → `Issue` CRs via polling. Operator only; no agent pods yet.
- [ ] **Phase 2 — Agents.** One pod per `Issue`, runs `opencode` against a clone of the repo, pushes a branch, opens a PR via the GitHub App.
- [ ] **Phase 3+ (later).** Webhooks, credential rotation, multi-tenancy, HA leader election, Prometheus dashboards, Helm chart.

**Do not implement work from a future phase** unless explicitly asked. If a Phase 1 design choice paints us into a corner for Phase 2, raise it — do not silently pre-build for Phase 2.

## Already in place

- `scripts/minikube/start-minikube.sh` — `minikube start --profile=agent-swarm`, enables the `registry` addon, patches CoreDNS forwarders, starts the host-side registry proxy.
- `scripts/minikube/stop-minikube.sh` — tears down the cluster and the registry proxy container.
- `Makefile` — exposes `make setup`, `make start-minikube`, `make stop-minikube`.
- `.secrets/github-app.yml` — GitHub App Secret manifest, gitignored. Apply with `kubectl apply -f .secrets/github-app.yml`.
- `.secrets/github-app.yml.example` — committable template using `stringData:` for new contributors.
- `.gitignore` — ignores `.secrets/*` but whitelists `*.example`.

The project tree is otherwise empty. Kubebuilder has **not** been initialized yet — that is one of the next steps.

## Project conventions (intended; reconfirm when locking in)

These were the design choices from the previous attempt. Treat as defaults, not facts — confirm with the user before committing them to `PROJECT`/`go.mod`:

- **Module path:** `github.com/pontuscurtsson/agent-swarm` (was `k8s-agent-automation` previously).
- **CRD group / domain:** `agentswarm.dev`.
- **API version:** start at `v1alpha1`. Bump on breaking changes.
- **Kinds:** `Repository`, `Issue`. `Repository` owns `Issue`s via `ownerReferences`; deleting a `Repository` cascades.
- **Resource naming:** `Issue` CRs are named `<repository-cr-name>-<issue-number>` to keep names deterministic and reconciliation idempotent.
- **GitHub auth:** GitHub App. App ID + installation ID + private key in a Secret referenced by `Repository.spec.auth.githubApp.secretRef`. No PAT path.
- **Agent runtime (Phase 2):** `opencode` CLI authenticated via an opencode.ai/zen API key, mounted from a Secret. No subscription-OAuth hacks.

## Architecture (target)

```
                ┌────────────────────┐
                │   Repository CR    │  spec: owner, repo, syncIntervalSeconds, auth
                └─────────┬──────────┘
                          │ owns
                          ▼
                ┌────────────────────┐
                │      Issue CR      │  spec: number, title, body, labels
                └────────────────────┘  status: phase, agentPodRef (Phase 2)
```

Controllers:

- `RepositoryController` — owns the polling loop. On reconcile: list GH issues for the repo, then create/update/delete child `Issue` CRs to match. Requeue after `spec.syncIntervalSeconds`.
- `IssueController` — Phase 1: stub that sets `status.phase`. Phase 2: provisions the agent pod and tracks its lifecycle through to PR URL.

## What not to do (yet)

- **No webhooks.** Polling only. Webhook support is Phase 3+.
- **No leader election concerns.** Single-replica deployment is fine.
- **No multi-tenancy hardening.** One operator, one team-owned cluster.
- **No metrics scaffolding beyond what Kubebuilder generates.** Dashboards come later.
- **No Helm chart.** Kustomize is enough for now.
- **No PAT auth path.** GitHub App only.
- **No subscription-based agent auth.** Zen API key only.
- **No wholesale copying from `~/Work/chas-exam`.** It is a reference for shape and decisions, not a source for code. Re-derive each piece intentionally so the user owns it.

## When working on this codebase

- **Verify versions before suggesting code.** `controller-runtime` and Kubebuilder APIs shift between versions. Check what is pinned in `go.mod` (once it exists) and read the matching docs before recommending a snippet.
- **`PROJECT` is the source of truth** for which Kinds and scaffolds exist once `kubebuilder init` has been run. Read it when in doubt.
- **No new tools without asking.** The toolchain is Docker + kubectl + kubebuilder + make + minikube + go. Helm/asdf/mise/sops/etc. require explicit approval.
- **Phase discipline.** If a request seems to span phases, name the phase mismatch and confirm scope before writing code.
