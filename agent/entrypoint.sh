#!/usr/bin/env bash
set -euo pipefail

workspace_dir="${AGENT_WORKSPACE:-/workspace/repo}"
issue_number="${ISSUE_NUMBER:-unknown}"
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

prompt="In the current repository, append exactly one line to .agent-output that says: hello from opencode agent for issue #${issue_number}. Do not modify any other files."

opencode run --dir "${workspace_dir}" --model "${opencode_model}" "${prompt}"

if [[ ! -f "${output_file}" ]]; then
  printf '.agent-output was not created by opencode\n' >&2
  exit 1
fi

if ! grep -Fq "hello from opencode agent for issue #${issue_number}" "${output_file}"; then
  printf '.agent-output does not contain expected issue marker\n' >&2
  exit 1
fi

printf 'agent completed\n' > /workspace/agent-result.txt
