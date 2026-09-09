# github plugin recipes. Invoke as `just github <recipe>`.
#
# supergraph query writes errors to stderr and nothing to stdout on failure,
# so every recipe here is a `set -euo pipefail` bash shebang recipe — a plain
# `supergraph query ... | jq '.x'` prints null and exits 0 without it.
#
# source_directory() (NOT justfile_directory(), which resolves to the ROOT
# justfile's dir even inside a module) gives this module's own directory, so
# --queries-dir stays anchored here regardless of caller cwd.
sg := env_var_or_default("SUPERGRAPH_BIN", "supergraph")

# github pr(owner/repo, number): PR fields (flattened node, no .repository)
pr-status owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op pr --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var number={{ number }} \
        | jq '.pr'

# github checkRunsForPR: flattened list of check runs on a PR's head commit
pr-checks owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op checkRunsForPR --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var number={{ number }} \
        | jq '[.checkRunsForPR.repository.pullRequest.commits.nodes[0].commit.checkSuites.nodes[].checkRuns.nodes[]]'

# github openPRs: open pull requests
open-prs owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op openPRs --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.openPRs.repository.pullRequests.nodes'

# github openIssues: open issues
open-issues owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op openIssues --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.openIssues.repository.issues.nodes'

# github myClaimed(owner/repo, login): issues assigned to login
my-claimed owner_repo login:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op myClaimed --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var login="{{ login }}" \
        | jq '.myClaimed.repository.issues.nodes'

# github prsAwaitingReview: PRs waiting on review
prs-awaiting-review owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op prsAwaitingReview --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.prsAwaitingReview.repository.pullRequests.nodes'

# github repoLabels: labels defined on a repo
repo-labels owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op repoLabels --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.repoLabels.repository.labels.nodes'

# github issueComments(owner/repo, number): comments on an issue
issue-comments owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op issueComments --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var number={{ number }} \
        | jq '.issueComments.repository.issue.comments.nodes'

# github paneForBranch(owner/repo, branch): ref target oid for a branch
branch-ref owner_repo branch:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op paneForBranch --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        --var owner="$owner" --var repo="$repo" --var branch="{{ branch }}" \
        | jq '.paneForBranch.repository.ref'

# NOTE: plugins/github/queries/issue.graphql (the executor variant) is
# shadowed — `--op issue --plugin github` always routes to the core-typed op
# below, so the executor file is unreachable from the CLI and has no recipe.

# --- github (cached/typed) ---

# core-typed issue(key=issue:owner/repo#n): cache-only, no upstream hop
issue-cached owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op issue --plugin github \
        --queries-dir "{{ source_directory() }}/queries/typed" \
        --var key="issue:{{ owner_repo }}#{{ number }}" \
        | jq '.issue'

# core-typed pullRequest(key=pr:owner/repo#n): cache-only, no upstream hop
pr-cached owner_repo number:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op pullRequest --plugin github \
        --queries-dir "{{ source_directory() }}/queries/typed" \
        --var key="pr:{{ owner_repo }}#{{ number }}" \
        | jq '.pullRequest'

# core-typed issuesForRepo(owner, repo)
issues-cached owner_repo:
    #!/usr/bin/env bash
    set -euo pipefail
    slug="{{ owner_repo }}"; owner="${slug%%/*}"; repo="${slug##*/}"
    {{ sg }} query --op issuesForRepo --plugin github \
        --queries-dir "{{ source_directory() }}/queries/typed" \
        --var owner="$owner" --var repo="$repo" \
        | jq '.issuesForRepo'

# --- mutations ---

# label a PR/issue (owner/repo, number, comma-separated labels)
pr-label owner_repo number labels:
    #!/usr/bin/env bash
    set -euo pipefail
    gh issue edit "{{ number }}" --repo "{{ owner_repo }}" --add-label "{{ labels }}"

# comment on a PR
pr-comment owner_repo number body:
    #!/usr/bin/env bash
    set -euo pipefail
    gh pr comment "{{ number }}" --repo "{{ owner_repo }}" --body "{{ body }}"

# comment on an issue
issue-comment owner_repo number body:
    #!/usr/bin/env bash
    set -euo pipefail
    gh issue comment "{{ number }}" --repo "{{ owner_repo }}" --body "{{ body }}"

# raw escape hatch for a github op without a recipe: no jq projection applied
q op *args:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ sg }} query --op {{ op }} --plugin github \
        --queries-dir "{{ source_directory() }}/queries" \
        {{ args }}
