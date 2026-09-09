Feature: GitHub typed Query + the PRD first cross-plugin join
  As the owner
  I want issue #N, its PR, and every tmux pane + claude session touching it in ONE GraphQL query
  So that "what is everything touching issue #N" is one answer, not five lookups

  # These fields are served from the github plugin's SQLite cache ONLY: a hit returns the node with
  # zero upstream calls; a miss returns null/[] (warming stays the proxy `/plugins/github/graphql`
  # path). The join goes issue# ↔ branch ↔ tmux pane ↔ claude session through ONE shared convention
  # (internal/issuekey.FromBranch, anchored `^issue-?(\d+)([/-]|$)`) PLUS the cached PR
  # body/title closing-keyword links (close/fix/resolve #N). @local runs against the fake GitHub server plus
  # in-memory fake claude/tmux stores — no PAT. @live needs env GITHUB_TOKEN + GITHUB_ORG and is
  # @pending until run against live GitHub. See docs/edr/github-query.md.

  # ---------- Cache-only typed reads ----------

  @github @local @AC-GHQ-HIT
  Scenario: A cached issue is served with no upstream call
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is already in the store, fresh, with title "wire the join"
    When `issue` is queried for key `issue:o/r#5`
    Then the complete stored issue node is returned with title "wire the join"
    And the fake GitHub server records zero requests during the query

  @github @local @AC-GHQ-MISS
  Scenario: A cache miss returns null with no upstream call
    Given a supergraph server started with the github plugin and data dir <tmp>
    And no node for key `issue:o/r#404` is in the store
    When `issue` is queried for key `issue:o/r#404`
    Then the result is null
    And the fake GitHub server records zero requests during the query

  @github @local @AC-GHQ-LIST
  Scenario: issuesForRepo serves the warmed list with no upstream call, and empty for a cold repo
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the `openIssues` op has been warmed for `o/r` with issues #5, #6, #7
    When `issuesForRepo` is queried for owner "o" repo "r"
    Then exactly issues #5, #6, #7 are returned
    And the fake GitHub server records zero requests during the query
    When `issuesForRepo` is queried for owner "o" repo "never-warmed"
    Then an empty list is returned

  @github @local @AC-GHQ-ASSIGNEES
  Scenario: issuesForRepo exposes state and assignees from cache for the dispatcher filter
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is in the store assigned to "alice" in state "open"
    When `issuesForRepo` is queried for owner "o" repo "r"
    Then issue #5 has state "open" and assignee "alice"
    And the fake GitHub server records zero requests during the query
    # The dispatcher's "open, unassigned, no grinding label" filter is one cached
    # query: state + assignees (empty = unassigned) + labels all served from cache.

  @github @local @AC-GHQ-HEADREF
  Scenario: A pull request exposes its head branch name
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#8` is already in the store with headRefName "issue5/spike-core"
    When `pullRequest` is queried for key `pr:o/r#8`
    Then the returned pull request's headRefName is "issue5/spike-core"

  # ---------- The PRD first join (issue# ↔ branch ↔ pane ↔ session) ----------

  @github @local @AC-GHQ-JOIN-HIT
  Scenario: One query returns the pane and the session whose branch matches issue #N
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is in the store
    And a tmux pane `pane:issue5-spike:0.0@boxA` is on branch "issue5/spike-core"
    And a claude session `sess-1` is on branch "issue5/spike-core"
    When `issue` is queried for key `issue:o/r#5` selecting `tmuxPanes` and `claudeSessions`
    Then the response tmuxPanes contains pane key `pane:issue5-spike:0.0@boxA`
    And the response claudeSessions contains session id `sess-1`

  @github @local @AC-GHQ-JOIN-EMPTY
  Scenario: The join is empty (not omitted, not an error) when no branch matches
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#9` is in the store
    And no tmux pane or claude session is on any branch deriving to issue 9
    When `issue` is queried for key `issue:o/r#9` selecting `tmuxPanes` and `claudeSessions`
    Then the response tmuxPanes is an empty list
    And the response claudeSessions is an empty list

  @github @local @AC-GHQ-JOIN-CONSISTENT
  Scenario: One shared derivation drives both sides of the join at a regex boundary
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#12` is in the store
    And a tmux pane `pane:issue12-foo:0.0@boxA` is on branch "issue12-foo"
    And a claude session `sess-12` is on branch "issue12-foo"
    When `issue` is queried for key `issue:o/r#12` selecting `tmuxPanes` and `claudeSessions`
    Then the response tmuxPanes contains pane key `pane:issue12-foo:0.0@boxA`
    And the response claudeSessions contains session id `sess-12`
    # A single boundary branch ("issue12-foo", the `issue12-` no-slash form) attaching on
    # both sides proves the claude filter and the tmux branch set use the identical shared
    # derivation (internal/issuekey — module-root, not plugins/internal, so graph/ can import it;
    # `^issue-?(\d+)([/-]|$)`); if the two plugins
    # used different regexes they could not both resolve this boundary form to 12.

  @github @local @AC-GHQ-BRANCH-REGEX
  Scenario Outline: The shared branch→issue regex matches and rejects the agreed forms
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#<n>` is in the store
    And a tmux pane `pane:p:0.0@boxA` is on branch "<branch>"
    When `issue` is queried for key `issue:o/r#<n>` selecting `tmuxPanes`
    Then the response tmuxPanes <expectation> pane key `pane:p:0.0@boxA`

    Examples: matches (branch derives to issue <n>)
      | branch            | n  | expectation |
      | issue1/spike-core | 1  | contains    |
      | issue-12          | 12 | contains    |
      | issue12-foo       | 12 | contains    |

    Examples: rejects (branch derives to no issue, so never attaches)
      | branch     | n | expectation      |
      | fix/issue3 | 3 | does not contain |
      | myissue4   | 4 | does not contain |

  # ---------- PR body/title closing-keyword links (owner 2026-09-05) ----------

  @github @local @AC-GHQ-PR-CLOSES
  Scenario: A PR whose body closes #N attaches that PR's branch pane/session to issue #N
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#20` is in the store
    And pull request `pr:o/r#21` is in the store with headRefName "hotfix/no-issue-prefix" and body "closes #20"
    And a tmux pane `pane:hotfix:0.0@boxA` is on branch "hotfix/no-issue-prefix"
    And a claude session `sess-20` is on branch "hotfix/no-issue-prefix"
    When `issue` is queried for key `issue:o/r#20` selecting `tmuxPanes` and `claudeSessions`
    Then the response tmuxPanes contains pane key `pane:hotfix:0.0@boxA`
    And the response claudeSessions contains session id `sess-20`
    # The branch "hotfix/no-issue-prefix" derives to NO issue number, so the only path that
    # attaches it to #20 is the cached PR body's closing keyword "closes #20".

  @github @local @AC-GHQ-MENTION-NOLINK
  Scenario: A bare #N mention with no closing keyword does NOT link the PR's branch
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#22` is in the store
    And pull request `pr:o/r#23` is in the store with headRefName "hotfix/mention-only" and body "see #22 for context"
    And a tmux pane `pane:mention:0.0@boxA` is on branch "hotfix/mention-only"
    When `issue` is queried for key `issue:o/r#22` selecting `tmuxPanes`
    Then the response tmuxPanes is an empty list
    # "#22" without a close/fix/resolve keyword is a mention, not a link (negative control
    # for AC-GHQ-PR-CLOSES); the branch also has no `issue-?N` prefix, so nothing attaches.

  @github @local @AC-GHQ-JOIN-VIA-PR-HEADREF
  Scenario: A PR head branch reaches the join only via the cached PR head-branch contributor
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is in the store
    And pull request `pr:o/r#8` is in the store with headRefName "feature/widget" and body "Closes #5"
    And a tmux pane `pane:widget:0.0@boxA` is on branch "feature/widget"
    And a claude session `sess-5` is on branch "feature/widget"
    When `issue` is queried for key `issue:o/r#5` selecting `tmuxPanes` and `claudeSessions`
    Then the response tmuxPanes contains pane key `pane:widget:0.0@boxA`
    And the response claudeSessions contains session id `sess-5`
    # "feature/widget" derives to NO issue, so neither the tmux-session contributor nor
    # the claude-issue-number filter can attach it. The branch reaches #5 only because
    # the cached PR's headRefName is added to the branch set by the closing-keyword scan
    # of its body ("Closes #5") — the PR head-branch contributor in isolation. Distinct
    # from AC-GHQ-JOIN-HIT, where the branch self-derives (issue5/...) and the pane's own
    # tmux session and the claude session each supply it directly.

  @github @local @AC-GHQ-P95
  Scenario: The one-query PRD join is sub-second at p95 on a warm cache over 20 samples
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is in the store with a matching pane and session on branch "issue5/spike-core"
    When this exact query is run 20 times against the warm cache:
      """
      { issue(key: "issue:o/r#5") { number tmuxPanes { key } claudeSessions { sessionId } } }
      """
    Then the p95 latency over the 20 paired samples is under 1s
    And evidence is captured: "p95 one-query PRD join < 1s over 20 warm samples, via paired timestamps"

  # ---------- PR sidebar-parity fields (#26) ----------

  @github @local @AC-GHPR-FIELDS
  Scenario: A PR warmed through the pr op exposes the four sidebar fields verbatim
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#8` is warmed through the `pr` op with draft true, reviewDecision "APPROVED", statusCheckRollup "SUCCESS", mergeStateStatus "CLEAN"
    When `pullRequest` is queried for key `pr:o/r#8`
    Then the returned pull request's draft is true
    And the returned pull request's reviewDecision is "APPROVED"
    And the returned pull request's statusCheckRollup is "SUCCESS"
    And the returned pull request's mergeStateStatus is "CLEAN"
    And the fake GitHub server records zero requests during the query

  @github @local @AC-GHPR-NULLS
  Scenario: Missing reviewDecision/statusCheckRollup are null; a seeded mergeStateStatus stays verbatim
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#30` is in the store with draft false, reviewDecision null, statusCheckRollup null, mergeStateStatus null
    And pull request `pr:o/r#31` is in the store with draft false, reviewDecision null, statusCheckRollup null, mergeStateStatus "UNKNOWN"
    When `pullRequest` is queried for key `pr:o/r#30`
    Then the returned pull request's draft is false
    And the returned pull request's reviewDecision is null
    And the returned pull request's statusCheckRollup is null
    When `pullRequest` is queried for key `pr:o/r#31`
    Then the returned pull request's mergeStateStatus is "UNKNOWN"
    # mergeStateStatus="UNKNOWN" must survive verbatim — never coerced to null or "CLEAN".

  @github @local @AC-GHPR-ROLLUP-RAW
  Scenario: statusCheckRollup is the raw upstream rollup state, never derived from check runs
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#40` is in the store with draft false, reviewDecision null, statusCheckRollup "FAILURE", mergeStateStatus null
    And cached checkRun nodes for `pr:o/r#40` all report success
    When `pullRequest` is queried for key `pr:o/r#40`
    Then the returned pull request's statusCheckRollup is "FAILURE"
    # The rollup state is "FAILURE" while every one of its cached checkRun nodes is
    # seeded success, so a value derived from the check-run list would read "SUCCESS":
    # "FAILURE" proves the field is the upstream rollup state read verbatim.

  @github @local @AC-GHPR-LIST
  Scenario: pullRequestsForRepo serves warmed open PRs with the four fields, empty for cold, null for a never-warmed key
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the `openPRs` op has been warmed for `o/r` with PRs #8, #9 carrying the four sidebar fields
    When `pullRequestsForRepo` is queried for owner "o" repo "r"
    Then exactly pull requests #8, #9 are returned
    And each returned pull request has draft, reviewDecision, statusCheckRollup and mergeStateStatus populated
    And the fake GitHub server records zero requests during the query
    When `pullRequestsForRepo` is queried for owner "o" repo "never-warmed"
    Then an empty pull request list is returned
    When `pullRequest` is queried for key `pr:o/r#999`
    Then the pull request result is null
    And the fake GitHub server records zero requests during the query

  @github @local @AC-GHPR-CHECKRUN-EVENT
  Scenario: A check_run webhook purges the PR and the next read serves the refetched rollup
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#8` is warmed through the `pr` op with draft false, reviewDecision "APPROVED", statusCheckRollup "SUCCESS", mergeStateStatus "CLEAN"
    And a websocket subscription to `checkRunUpdated` is open
    And upstream `pr:o/r#8` changes statusCheckRollup to "FAILURE"
    When a signed `check_run` webhook naming pull request #8 is received
    Then exactly one envelope keyed `pr:o/r#8` is pushed on the `checkRunUpdated` subscription
    And `pr:o/r#8` is purged from the cache
    When `pr:o/r#8` is re-warmed and queried again
    Then the returned pull request's statusCheckRollup is "FAILURE"

  @github @local @AC-GHPR-PR-EVENT
  Scenario: A pull_request webhook purges the PR and the next read serves the new draft and mergeStateStatus
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#8` is warmed through the `pr` op with draft false, reviewDecision "APPROVED", statusCheckRollup "SUCCESS", mergeStateStatus "CLEAN"
    And a websocket subscription to `checkRunUpdated` is open
    And upstream `pr:o/r#8` changes draft to true
    And upstream `pr:o/r#8` changes mergeStateStatus to "BLOCKED"
    When a signed `pull_request` webhook naming PR #8 is received
    Then exactly one envelope keyed `pr:o/r#8` is pushed on the `checkRunUpdated` subscription
    And `pr:o/r#8` is purged from the cache
    When `pr:o/r#8` is re-warmed and queried again
    Then the returned pull request's draft is true
    And the returned pull request's mergeStateStatus is "BLOCKED"

  @github @local @AC-GHPR-REVIEW-EVENT
  Scenario: A pull_request_review webhook purges the PR and the next read serves the new reviewDecision
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#8` is warmed through the `pr` op with draft false, reviewDecision "REVIEW_REQUIRED", statusCheckRollup "SUCCESS", mergeStateStatus "CLEAN"
    And a websocket subscription to `checkRunUpdated` is open
    And upstream `pr:o/r#8` changes reviewDecision to "APPROVED"
    When a signed `pull_request_review` webhook naming PR #8 is received
    Then exactly one envelope keyed `pr:o/r#8` is pushed on the `checkRunUpdated` subscription
    And `pr:o/r#8` is purged from the cache
    When `pr:o/r#8` is re-warmed and queried again
    Then the returned pull request's reviewDecision is "APPROVED"

  @github @local @AC-GHPR-REVALIDATE
  Scenario: A reconcile revalidate pass keeps the four sidebar fields verbatim
    Given a supergraph server started with the github plugin and data dir <tmp>
    And pull request `pr:o/r#8` is warmed through the `pr` op with draft true, reviewDecision "APPROVED", statusCheckRollup "SUCCESS", mergeStateStatus "CLEAN"
    When the reconcile revalidate pass runs once
    And `pullRequest` is queried for key `pr:o/r#8`
    Then the returned pull request's draft is true
    And the returned pull request's reviewDecision is "APPROVED"
    And the returned pull request's statusCheckRollup is "SUCCESS"
    And the returned pull request's mergeStateStatus is "CLEAN"

  @github @local @AC-GHPR-ADDONLY
  Scenario: The github schema change is additions-only
    Given the github-query change is applied
    When `git diff main -- plugins/github/schema/github.graphqls` is run
    Then the schema diff has no deleted or retyped lines

  # ---------- Guards ----------

  @github @local @AC-GHQ-ZEROCORE @AC-GHPR-ZEROCORE
  Scenario: The feature adds no core edit
    Given the github-query change is applied
    When `git diff --stat core/ server/` is run
    Then the diffstat is empty

  @github @local @AC-GHQ-LOC
  Scenario: The github plugin stays under the ratcheted LOC cap
    Given the github-query change is applied
    When `make loc-github` is run
    Then it exits zero against the cap raised to measured plus five percent
    And the github EDR budget table shows the new measured total and cap

  # ---------- Live parity ----------

  @github @live @AC-GHQ-LIVE-WARM
  Scenario: Against real GitHub, issuesForRepo matches the REST listing after a warm
    Given a supergraph server with a real GITHUB_TOKEN and a repo under GITHUB_ORG
    And the `openIssues` op is warmed through `/plugins/github/graphql` for that repo
    When `issuesForRepo` is queried for that repo
    Then the returned issue numbers equal the open issues the GitHub REST API lists
    And evidence is captured: "issuesForRepo parity with GitHub REST, via a screenshot"
