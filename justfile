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
set positional-arguments

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
#
# [no-cd]: git mutations act on the CALLER's repo, not the justfile's own
# directory. Once installed globally (`just -g`), the justfile's directory is
# ~/.local/share/supergraph — not a git repo — so these three recipes must
# run with the invocation-time cwd. Every query recipe above keeps the
# default cd-to-justfile-dir behaviour; do not add [no-cd] there.

# create a git worktree for a branch at a path
[no-cd]
worktree-create branch path:
    #!/usr/bin/env bash
    set -euo pipefail
    git worktree add "$2" -b "$1"

# remove a git worktree
[no-cd]
worktree-remove path:
    #!/usr/bin/env bash
    set -euo pipefail
    git worktree remove "$1"

# delete a local and remote branch
[no-cd]
branch-delete branch:
    #!/usr/bin/env bash
    set -euo pipefail
    git branch -D "$1"
    git push origin --delete "$1"
