Feature: git plugin — local repo/worktree read model
  As the orchardist and the cross-source join
  I want configured git repos and their worktrees served as a live read model
  So that free-agent placement and issue/PR joins have real worktree state to point at

  # The git plugin watches a set of locally-configured repo roots (config: [plugins.git] repos =
  # [...], reconcileIntervalSeconds). For each root it reads `git worktree list --porcelain` to
  # build Repo.worktrees; ahead/behind comes from the upstream comparison `git status -sb` reports
  # (null when the branch has no upstream, never 0/0 as a stand-in for "no upstream"); slug is the
  # `origin` remote's owner/name, falling back to the root's directory basename when there is no
  # origin. @local runs against REAL git repos created per scenario in a temp dir (`git init`, real
  # commits, real `git worktree add`) and torn down after — hermetic, no fixtures, no credentials.
  # @tmux scenarios additionally need a real tmux server on a private `-L` socket, same pattern as
  # tmux.feature. Omitting [plugins.git], or repos = [], leaves the plugin dormant.

  # ---------- Worktree enumeration ----------

  @git @local @AC-GIT-WORKTREES
  Scenario: Two configured repo roots each yield their own worktrees, matching git's own listing
    Given two configured git repo roots, each with a second worktree checked out on its own branch
    And one of the roots also has a worktree checked out with a detached HEAD
    When `repos` is queried for both hosts' repos
    Then each Repo's worktrees match that root's own `git worktree list --porcelain` output exactly
    And no worktree from one repo appears under the other repo
    And the detached-HEAD worktree reports a null branch

  # ---------- Ahead/behind: the null-vs-zero edge ----------

  @git @local @AC-GIT-AHEAD-BEHIND
  Scenario Outline: A worktree's ahead/behind reflects `git status -sb`, and no-upstream is null not zero
    Given a repo root with a worktree on a branch <branch_state>
    When `repos` is queried for that repo
    Then the worktree's ahead/behind is <expected>

    Examples:
      | branch_state                              | expected                    |
      | with an upstream 2 commits ahead, 3 behind | ahead 2 and behind 3        |
      | level with its upstream                    | ahead 0 and behind 0        |
      | with no upstream configured                | ahead null and behind null  |

  # ---------- Slug derivation ----------

  @git @local @AC-GIT-SLUG
  Scenario Outline: A repo's slug comes from its origin remote, falling back to the directory name
    Given a repo root <origin_state>
    When `repos` is queried for that repo
    Then the repo's slug is <expected_slug>

    Examples:
      | origin_state                                          | expected_slug             |
      | with an `origin` remote `git@github.com:acme/widget.git` | "acme/widget"           |
      | with no `origin` remote                               | its directory basename    |

  # ---------- Cross-plugin join: tmux session ----------

  @git @local @tmux @AC-GIT-TMUX-JOIN
  Scenario: A worktree's tmuxSession is populated only when a real tmux session lives on its path
    Given a repo root with two worktrees, "attended" and "unattended"
    And a tmux server on a private socket with a session whose current path is the "attended" worktree
    When a supergraph server watches both that repo and that tmux socket
    And `repos` is queried for that repo
    Then the "attended" worktree's tmuxSession is not null
    And the "unattended" worktree's tmuxSession is null

  # ---------- Cross-plugin join: issue / PR ----------

  # The matching rule (graph/git.resolvers.go + graph/git_map.go): a worktree's
  # branch derives an issue number N via the shared internal/issuekey grammar
  # (branch "issueN/..." or "issue-N" etc). That N forms the github plugin's
  # cache key issue:<owner>/<repo>#<N> (owner/repo from the repo's slug), read
  # cache-only. For pullRequest, the same derived N forms pr:<owner>/<repo>#<N>,
  # but a hit there is only used when its headRefName equals the worktree's own
  # branch (issue #N and PR #N are different nodes in one repo) — otherwise the
  # join falls back to scanning the repo's warm-cached PRs for one whose
  # headRefName equals the branch, tie-broken on the smallest PR number. Neither
  # path filters by state: a closed or merged PR on the branch is still returned.
  @git @local @github @AC-GIT-ISSUE-JOIN
  Scenario: A worktree resolves the GitHub issue its branch encodes, from the warm cache
    Given a repo root whose origin is `acme/widget`, with a worktree on branch "issue7/some-slug"
    And GitHub issue #7 in `acme/widget` is warm-cached with title "spike: core work"
    When `repos` is queried for that repo
    Then the worktree's issue is GitHub issue #7 with title "spike: core work"

  @git @local @github @AC-GIT-ISSUE-JOIN
  Scenario: A worktree on a branch encoding no issue number resolves a null issue
    Given a repo root whose origin is `acme/widget`, with a worktree on branch "ci/pr-cheap"
    When `repos` is queried for that repo
    Then the worktree's issue is null

  @git @local @github @AC-GIT-PR-JOIN
  Scenario: A worktree on a non-issue branch resolves the pull request via the headRefName fallback
    Given a repo root whose origin is `acme/widget`, with a worktree on branch "hotfix-login"
    And GitHub PR #12 in `acme/widget` is warm-cached with headRefName "hotfix-login"
    When `repos` is queried for that repo
    Then the worktree's pullRequest is GitHub PR #12

  @git @local @github @AC-GIT-PR-JOIN
  Scenario: A closed pull request on the branch is still returned, unfiltered by state
    Given a repo root whose origin is `acme/widget`, with a worktree on branch "issue9/hotfix"
    And GitHub PR #9 in `acme/widget` is warm-cached with headRefName "issue9/hotfix" and state "CLOSED"
    When `repos` is queried for that repo
    Then the worktree's pullRequest is GitHub PR #9 with state "CLOSED"

  # ---------- Reconcile / staleness ----------

  @git @local @AC-GIT-RECONCILE
  Scenario: A worktree removed on disk is still reported, marked stale, after the next reconcile
    Given a supergraph server watching a repo root with a second worktree
    When that worktree is removed with `git worktree remove`
    And the next reconcile runs
    Then `repos` still returns that worktree's path
    And its staleSince is not null

  # ---------- Subscription ----------

  @git @local @AC-GIT-SUBSCRIBE
  Scenario: Adding a worktree pushes a worktreeUpdated event on the subscription
    Given a supergraph server watching a repo root
    And I run `supergraph subscribe worktreeUpdated --once` against it in the background
    When a second worktree is added to that root
    Then the worktree subscribe process exits 0 within 5s
    And its stdout is exactly one compact JSON line naming type `git.worktree.updated`

  # ---------- hostId filtering ----------

  @git @local @AC-GIT-HOSTID
  Scenario: repos(hostId:) filters to the configured host and returns nothing for an unknown one
    Given a supergraph server watching a repo root with hostId "test"
    When `repos(hostId: "test")` is queried
    Then at least one repo row is returned
    When `repos(hostId: "nope")` is queried
    Then zero repo rows are returned

  # ---------- Dormant ----------

  @git @local @AC-GIT-DORMANT
  Scenario: A server with no [plugins.git] section starts clean and reports no repos
    Given a supergraph server started with no `[plugins.git]` section configured
    When `repos` is queried
    Then zero repo rows are returned
