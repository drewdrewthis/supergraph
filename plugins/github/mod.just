# github plugin recipes. Invoke as `just github <recipe>`.
#
# supergraph query writes errors to stderr and nothing to stdout on failure,
# so every recipe here is a `set -euo pipefail` bash shebang recipe — a plain
# `supergraph query ... | jq '.x'` prints null and exits 0 without it.
#
# source_directory() (NOT justfile_directory(), which resolves to the ROOT
# justfile's dir even inside a module) gives this module's own directory, so
# --queries-dir stays anchored here regardless of caller cwd.
#
# set positional-arguments: recipe parameters are passed to the bash
# shebang as $1, $2, ... instead of being interpolated as `{{ param }}` text
# into the script — a param containing a double quote can't break out of
# the shell quoting because it's never spliced into source text at all.
# {{ source_directory() }} and {{ sg }} are just-level values, not recipe
# parameters, so they stay as `{{ }}` interpolation.
set positional-arguments

sg := env_var_or_default("SUPERGRAPH_BIN", "supergraph")

# github pr(owner/repo, number): PR status fields (body omitted — use `q pr` for the raw node)
pr-status owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op pr --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var number="$2" \
        | jq '.pr | {number, title, state, merged, url, headRefName, baseRefName, updatedAt, labels: [.labels.nodes[].name]}'

# github checkRunsForPR: flattened list of check runs on a PR's head commit
pr-checks owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op checkRunsForPR --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var number="$2" \
        | jq '[.checkRunsForPR.repository.pullRequest.commits.nodes[0].commit.checkSuites.nodes[].checkRuns.nodes[]]'

# github openPRs: open pull requests
open-prs owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op openPRs --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.openPRs.repository.pullRequests.nodes'

# github openIssues: open issues
open-issues owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op openIssues --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.openIssues.repository.issues.nodes'

# github myClaimed(owner/repo, login): issues assigned to login
my-claimed owner_repo login:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op myClaimed --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var login="$2" \
        | jq '.myClaimed.repository.issues.nodes'

# github prsAwaitingReview: PRs waiting on review
prs-awaiting-review owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op prsAwaitingReview --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.prsAwaitingReview.repository.pullRequests.nodes'

# github repoLabels: labels defined on a repo
repo-labels owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op repoLabels --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.repoLabels.repository.labels.nodes'

# github issueComments(owner/repo, number): comments on an issue
issue-comments owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op issueComments --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var number="$2" \
        | jq '.issueComments.repository.issue.comments.nodes'

# github paneForBranch(owner/repo, branch): ref target oid for a branch
branch-ref owner_repo branch:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op paneForBranch --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var branch="$2" \
        | jq '.paneForBranch.repository.ref'

# NOTE: plugins/github/queries/issue.graphql has no recipe here, but DO NOT
# DELETE IT. `--op issue --plugin github` always routes to the core-typed op
# below (cmd/supergraph/query.go coreTypedOps), so the file is unreachable
# from the CLI only. It is still load-bearing SERVER-side: executor.go embeds
# queries/*.graphql into p.ops, and proxy.go's canonicalBody looks up
# p.ops["issue"] to re-read a landed issue through its GraphQL point op.
# Removing the file makes that lookup miss and silently falls back to the REST
# body — reintroducing the empty-labels bug PR #32 fixed.

# --- github (cached/typed) ---

# core-typed issue(key=issue:owner/repo#n): cache-only, no upstream hop
issue-cached owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op issue --plugin github \
        --queries-dir "{{ source_directory() }}/queries/typed" \
        --var key="issue:$1#$2" \
        | jq '.issue'

# core-typed pullRequest(key=pr:owner/repo#n): cache-only, no upstream hop
pr-cached owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op pullRequest --plugin github \
        --queries-dir "{{ source_directory() }}/queries/typed" \
        --var key="pr:$1#$2" \
        | jq '.pullRequest'

# core-typed issuesForRepo(owner, repo)
issues-cached owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="$1"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op issuesForRepo --plugin github \
        --queries-dir "{{ source_directory() }}/queries/typed" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.issuesForRepo'

# --- mutations ---

# label a PR/issue (owner/repo, number, comma-separated labels)
pr-label owner_repo number labels:
    #!/usr/bin/env bash
    set -euo pipefail
    gh issue edit "$2" --repo "$1" --add-label "$3"

# comment on a PR
pr-comment owner_repo number body:
    #!/usr/bin/env bash
    set -euo pipefail
    gh pr comment "$2" --repo "$1" --body "$3"

# comment on an issue
issue-comment owner_repo number body:
    #!/usr/bin/env bash
    set -euo pipefail
    gh issue comment "$2" --repo "$1" --body "$3"

# raw escape hatch for a github op without a recipe: no jq projection applied
q op *args:
    #!/usr/bin/env bash
    set -euo pipefail
    op="$1"
    shift
    queries_dir="{{ source_directory() }}/queries"
    case "$op" in
        issue | pullRequest | issuesForRepo) queries_dir="{{ source_directory() }}/queries/typed" ;;
    esac
    {{ sg }} query --op "$op" --plugin github \
        --queries-dir "$queries_dir" \
        "$@"
