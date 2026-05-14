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

output_file="${workspace_dir}/.agent-output"

export OPENCODE_DISABLE_AUTOUPDATE=true
export OPENCODE_DISABLE_DEFAULT_PLUGINS=true
export OPENCODE_CONFIG_CONTENT='{"$schema":"https://opencode.ai/config.json","model":"{env:OPENCODE_MODEL}","provider":{"opencode":{"options":{"apiKey":"{env:OPENCODE_API_KEY}"}}},"share":"disabled"}'

prompt=$(cat <<EOF
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
- Before finishing, append a short summary of what you changed to .agent-output.
EOF
)

opencode run --dir "${workspace_dir}" --model "${opencode_model}" "${prompt}"

if [[ -z "$(git -C "${workspace_dir}" status --porcelain)" ]]; then
  printf 'opencode completed without producing any repository changes\n' >&2
  exit 1
fi

if [[ ! -f "${output_file}" ]]; then
  printf '.agent-output was not created by opencode\n' >&2
  exit 1
fi

printf 'agent completed\n' > /workspace/agent-result.txt
