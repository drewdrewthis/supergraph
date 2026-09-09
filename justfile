# Agent tooling layer over `supergraph query --op`: cross-plugin recipes here,
# per-plugin recipes in each plugin's own `mod.just` (next to its queries/
# dir). `make` stays build/test/CI; this file (and its modules) is the
# runtime surface agents call against a live `supergraph serve`. See
# README.md "Agent tooling (justfile)".
#
# just 1.58 has no glob module import, so each plugin module is listed
# explicitly below. `mod?` (not `mod`) tolerates a plugin dir with no
# mod.just yet.

# github plugin recipes: `just github <recipe>`
mod? github "plugins/github/mod.just"

# claude plugin recipes: `just claude <recipe>`
mod? claude "plugins/claude/mod.just"

# tmux plugin recipes: `just tmux <recipe>`
mod? tmux "plugins/tmux/mod.just"

# `mod?` + `set fallback` below make this a no-op on a box with no global library.

# global agent recipe library (drewdrewthis/just-recipes convention): `just global <recipe>`
mod? global '~/.claude/just/justfile'

set fallback

# sg is overridable so CI/dev can point at a freshly-built binary instead of
# whatever `supergraph` resolves to on PATH.
sg := env_var_or_default("SUPERGRAPH_BIN", "supergraph")

# list all recipes (bare `just` = `just --list`)
default:
    @just --list

# core health check over HTTP
health:
    #!/usr/bin/env bash
    set -euo pipefail
    curl -sf http://127.0.0.1:7788/health

# static drift gate: every module's --op/--plugin/--queries-dir resolves to a real .graphql file
check:
    scripts/just-check.sh

# --- git mutations ---

# create a git worktree for a branch at a path
worktree-create branch path:
    #!/usr/bin/env bash
    set -euo pipefail
    git worktree add "{{ path }}" -b "{{ branch }}"

# remove a git worktree
worktree-remove path:
    #!/usr/bin/env bash
    set -euo pipefail
    git worktree remove "{{ path }}"

# delete a local and remote branch
branch-delete branch:
    #!/usr/bin/env bash
    set -euo pipefail
    git branch -D "{{ branch }}"
    git push origin --delete "{{ branch }}"
