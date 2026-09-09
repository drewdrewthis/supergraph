# claude plugin recipes. Invoke as `just claude <recipe>`.
#
# set positional-arguments: recipe parameters are passed to the bash
# shebang as $1, $2, ... instead of being interpolated as `{{ param }}` text,
# so a param containing a double quote can't break out of the shell quoting.
# {{ source_directory() }} and {{ sg }} are just-level values, not recipe
# parameters, so they stay as `{{ }}` interpolation.
set positional-arguments

sg := env_var_or_default("SUPERGRAPH_BIN", "supergraph")

# claude sessionsForIssue(issueNumber): Claude sessions tied to an issue
sessions-for-issue number:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op sessionsForIssue --plugin claude \
        --queries-dir "{{ source_directory() }}/queries" \
        --var issueNumber="$1" \
        | jq '.claudeSessions'

# claude instances(hostId?): known Claude Code instances
instances host_id='':
    #!/usr/bin/env bash
    set -euo pipefail
    args=(--op instances --plugin claude \
        --queries-dir "{{ source_directory() }}/queries")
    if [ -n "$1" ]; then args+=(--var hostId="$1"); fi
    {{ sg }} query "${args[@]}" | jq '.claudeInstances'

# raw escape hatch for a claude op without a recipe: no jq projection applied
q op *args:
    #!/usr/bin/env bash
    set -euo pipefail
    op="$1"
    shift
    {{ sg }} query --op "$op" --plugin claude \
        --queries-dir "{{ source_directory() }}/queries" \
        "$@"
