# tmux plugin recipes. Invoke as `just tmux <recipe>`.
#
# set positional-arguments: recipe parameters are passed to the bash
# shebang as $1, $2, ... instead of being interpolated as `{{ param }}` text,
# so a param containing a double quote can't break out of the shell quoting.
# {{ source_directory() }} and {{ sg }} are just-level values, not recipe
# parameters, so they stay as `{{ }}` interpolation.
set positional-arguments

sg := env_var_or_default("SUPERGRAPH_BIN", "supergraph")

# tmux panes(hostId?): tracked tmux panes
panes host_id='':
    #!/usr/bin/env bash
    set -euo pipefail
    args=(--op panes --plugin tmux \
        --queries-dir "{{ source_directory() }}/queries")
    if [ -n "$1" ]; then args+=(--var hostId="$1"); fi
    {{ sg }} query "${args[@]}" | jq '.tmuxPanes'

# tmux sessions(hostId?): tracked tmux sessions
sessions host_id='':
    #!/usr/bin/env bash
    set -euo pipefail
    args=(--op sessions --plugin tmux \
        --queries-dir "{{ source_directory() }}/queries")
    if [ -n "$1" ]; then args+=(--var hostId="$1"); fi
    {{ sg }} query "${args[@]}" | jq '.tmuxSessions'

# tmux freeSlots(hostId?): unclaimed worker slots
free-slots host_id='':
    #!/usr/bin/env bash
    set -euo pipefail
    args=(--op freeSlots --plugin tmux \
        --queries-dir "{{ source_directory() }}/queries")
    if [ -n "$1" ]; then args+=(--var hostId="$1"); fi
    {{ sg }} query "${args[@]}" | jq '.freeSlots'

# tmux slotsForHost(hostId?): all slots for a host
slots-for-host host_id='':
    #!/usr/bin/env bash
    set -euo pipefail
    args=(--op slotsForHost --plugin tmux \
        --queries-dir "{{ source_directory() }}/queries")
    if [ -n "$1" ]; then args+=(--var hostId="$1"); fi
    {{ sg }} query "${args[@]}" | jq '.slots'

# tmux paneForBranch(branch): pane currently checked out on a branch
pane-for-branch branch:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op paneForBranch --plugin tmux \
        --queries-dir "{{ source_directory() }}/queries" \
        --var branch="$1" \
        | jq '.paneForBranch'

# raw escape hatch for a tmux op without a recipe: no jq projection applied
q op *args:
    #!/usr/bin/env bash
    set -euo pipefail
    op="$1"
    shift
    {{ sg }} query --op "$op" --plugin tmux \
        --queries-dir "{{ source_directory() }}/queries" \
        "$@"
