#!/usr/bin/env bash
set -euo pipefail

# Static drift gate for the justfile tooling layer (issue #37): every literal
# `--op NAME --plugin PLUGIN --queries-dir DIR` triple in a plugin module's
# recipe body must resolve to a real .graphql file. No server is started —
# this only reads the filesystem, so it belongs in CI right next to
# `just --fmt --check`.
#
# Recipes live in plugins/<plugin>/mod.just (not the root justfile, which
# only holds cross-plugin recipes with no --op of their own).

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

status=0
count=0

for mod_just in "$repo_root"/plugins/*/mod.just; do
    [ -f "$mod_just" ] || continue
    module_dir="$(dirname "$mod_just")"

    # Join backslash-continued lines into one logical line per recipe
    # command, so a --queries-dir on its own continuation line still pairs
    # with the --op / --plugin on the line above.
    joined="$(awk '
        /\\[[:space:]]*$/ {
            sub(/\\[[:space:]]*$/, " ")
            buf = buf $0
            next
        }
        { print buf $0; buf = "" }
    ' "$mod_just")"

    while IFS= read -r line; do
        # The generic `q` escape hatch passes a {{op}} placeholder, not a
        # literal name — nothing to resolve, so skip it.
        case "$line" in
            *'--op {{ op }}'*|*'--op {{op}}'*) continue ;;
        esac

        [[ "$line" =~ --op[[:space:]]+([A-Za-z0-9_]+) ]] || continue
        op="${BASH_REMATCH[1]}"

        [[ "$line" =~ --plugin[[:space:]]+([A-Za-z0-9_]+) ]] || continue
        plugin="${BASH_REMATCH[1]}"

        [[ "$line" =~ --queries-dir[[:space:]]+\"([^\"]+)\" ]] || continue
        dir="${BASH_REMATCH[1]}"
        # `just --fmt` normalizes interpolation spacing to "{{ expr }}", so
        # match loosely on the token rather than an exact literal.
        dir="$(echo "$dir" | sed -E "s#\\{\\{[[:space:]]*source_directory\\(\\)[[:space:]]*\\}\\}#${module_dir//\//\\/}#")"

        count=$((count + 1))
        path="$dir/$op.graphql"
        if [ -f "$path" ]; then
            echo "ok $plugin/$op"
        else
            echo "MISSING $plugin/$op -> $path" >&2
            status=1
        fi
    done <<< "$joined"
done

if [ "$count" -eq 0 ]; then
    echo "MISSING no --op recipes found under $repo_root/plugins/*/mod.just" >&2
    exit 1
fi

exit "$status"
