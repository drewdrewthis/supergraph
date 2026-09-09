# claude plugin recipes. Invoke as `just claude <recipe>`.
sg := env_var_or_default("SUPERGRAPH_BIN", "supergraph")

# claude sessionsForIssue(issueNumber): Claude sessions tied to an issue
sessions-for-issue number:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op sessionsForIssue --plugin claude \
        --queries-dir "{{ source_directory() }}/queries" \
        --var issueNumber={{ number }} \
        | jq '.claudeSessions'

# claude instances(hostId?): known Claude Code instances
instances host_id='':
    #!/usr/bin/env bash
    set -euo pipefail
    args=(--op instances --plugin claude \
        --queries-dir "{{ source_directory() }}/queries")
    if [ -n "{{ host_id }}" ]; then args+=(--var hostId="{{ host_id }}"); fi
    {{ sg }} query "${args[@]}" | jq '.claudeInstances'

# raw escape hatch for a claude op without a recipe: no jq projection applied
q op *args:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op {{ op }} --plugin claude \
        --queries-dir "{{ source_directory() }}/queries" \
        {{ args }}
