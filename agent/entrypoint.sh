#!/usr/bin/env bash
set -euo pipefail

workspace_dir="${AGENT_WORKSPACE:-/workspace/repo}"
issue_number="${ISSUE_NUMBER:-unknown}"
issue_title="${ISSUE_TITLE:-}"
issue_body="${ISSUE_BODY:-}"
issue_labels="${ISSUE_LABELS:-}"
opencode_model="${OPENCODE_MODEL:-opencode/gpt-5.4-mini}"

if [[ -z "${OPENCODE_API_KEY:-}" ]]; then
  printf 'missing required OPENCODE_API_KEY environment variable\n' >&2
  exit 1
fi

if [[ ! -d "${workspace_dir}" ]]; then
  printf 'workspace directory does not exist: %s\n' "${workspace_dir}" >&2
  exit 1
fi

if [[ ! -d "${workspace_dir}/.git" ]]; then
  printf 'workspace is not a git repository: %s\n' "${workspace_dir}" >&2
  exit 1
fi

export OPENCODE_DISABLE_AUTOUPDATE=true
export OPENCODE_DISABLE_DEFAULT_PLUGINS=true
# shellcheck disable=SC2016
# {env:VAR} is opencode's own template syntax — resolved by opencode at
# runtime, not bash. Single quotes are intentional: we want the literal
# {env:OPENCODE_MODEL} / {env:OPENCODE_API_KEY} strings in the JSON.
export OPENCODE_CONFIG_CONTENT='{"$schema":"https://opencode.ai/config.json","model":"{env:OPENCODE_MODEL}","provider":{"opencode":{"options":{"apiKey":"{env:OPENCODE_API_KEY}"}}},"share":"disabled"}'

prompt=$(
  cat << EOF
You are working in a checked-out git repository for GitHub issue #${issue_number}.

Issue title:
${issue_title}

Issue labels:
${issue_labels}

Issue body:
${issue_body}

Implement the issue request in this repository.

Constraints:
- Make the smallest focused change that solves the issue.
- Avoid unrelated refactors.
- Do not modify secrets or CI unless the issue explicitly requires it.
EOF
)

opencode run --dir "${workspace_dir}" --model "${opencode_model}" "${prompt}"

# No-op exit policy: if opencode produced zero file changes, fail.
# An Issue CR represents work to be done for a GitHub issue; "agent decided
# nothing was needed" is treated as a failure rather than a silent success,
# so the Issue lands in Failed with status.lastError pointing here instead of
# producing an empty PR or vanishing into Done with no audit trail. The
# publisher Job has its own defensive "nothing staged" branch for the edge
# case where opencode only touched .gitignored files.
if [[ -z "$(git -C "${workspace_dir}" status --porcelain)" ]]; then
  printf 'opencode completed without producing any repository changes\n' >&2
  exit 1
fi

printf 'agent completed\n' > /workspace/agent-result.txt
