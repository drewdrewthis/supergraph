Feature: PRD user-story and failure-surface acceptance criteria
  As the supergraph project owner
  I want every PRD §5 AC (S1-S6, F1-F8) represented as a pending scenario
  So that the plugin-tier work has a BDD contract before any plugin ships

  # These scenarios describe plugin-tier behavior (github, claude, tmux plugins
  # and cross-box federation) that does not exist yet in this core-tier spike.
  # All three steps below share one pending step definition (features/steps_prd_test.go):
  # it returns godog.ErrPending regardless of which quoted text it is called with, so
  # every scenario reports pending, never failing or silently skipped, until a plugin
  # implements the acceptance criterion it names.

  @pending @plugin-tier @S1
  Scenario: S1 — issue-open to PR-open stays under 15 minutes with zero human action
    Given the plugin tier for "github-orchestrator" is implemented
    When the acceptance criterion is exercised: "10 seeded issues run unattended through the review-clerk and tests gate"
    Then evidence is captured: "p95 issue-open to PR-open <= 15 min, via journald timestamps and PR-open screenshots"

  @pending @plugin-tier @S2
  Scenario: S2 — cross-box free-slot query answers warm in under 1 second
    Given the plugin tier for "tmux" is implemented
    When the acceptance criterion is exercised: "a cross-box free-slot query runs warm against a seeded realistic db"
    Then evidence is captured: "p95 query latency < 1s, via a timing screenshot"

  @pending @plugin-tier @S3
  Scenario: S3 — check-run webhook reaches a subscription push in under 1 second
    Given the plugin tier for "github" is implemented
    When the acceptance criterion is exercised: "a check-run webhook is received"
    Then evidence is captured: "p95 webhook-receipt to subscription-push < 1s, via paired journald lines"

  @pending @plugin-tier @S4
  Scenario: S4 — one query joins github and claude rows by issue, marking absent sources
    Given the plugin tier for "claude" is implemented
    When the acceptance criterion is exercised: "one query for issue #N runs against the graph"
    Then evidence is captured: "at least one row each from github and claude, source-tagged, absent source shows an explicit stale marker never omission, via a query result screenshot"

  @pending @plugin-tier @S5
  Scenario: S5 — the template plugin compiles in and serves without touching core
    Given the plugin tier for "template" is implemented
    When the acceptance criterion is exercised: "the template plugin is compiled in and `_health` is queried"
    Then evidence is captured: "`git diff --stat core/` is 0 files, via diff output and a `_health` screenshot"

  @pending @plugin-tier @S6
  Scenario: S6 — /health reports each plugin's last real event and marks a stalled one stale
    Given the plugin tier for "github" is implemented
    When the acceptance criterion is exercised: "a plugin stops emitting real events"
    Then evidence is captured: "within 60s `/health` shows state stale with growing lagSeconds derived from the plugin's last real event, via `/health` screenshots and a lag log"

  @pending @plugin-tier @F1
  Scenario: F1 Freshness — github and claude events become queryable in under 1 second
    Given the plugin tier for "ingest" is implemented
    When the acceptance criterion is exercised: "a github or claude event is ingested"
    Then evidence is captured: "p95 ingest-to-queryable < 1s, via paired log timestamps"

  @pending @plugin-tier @F2
  Scenario: F2 Reconcile — a dropped webhook is healed by the next hourly reconcile
    Given the plugin tier for "github" is implemented
    When the acceptance criterion is exercised: "a webhook is deliberately dropped and the next hourly reconcile runs, and an org backfill runs from an empty db"
    Then evidence is captured: "the dropped event is present after reconcile and backfill completes < 60 min, via a log and a timer"

  @pending @plugin-tier @F3
  Scenario: F3 New repo — a repo created after boot appears in the graph with zero config
    Given the plugin tier for "github" is implemented
    When the acceptance criterion is exercised: "a repo is created in the org after boot"
    Then evidence is captured: "the repo appears in the graph with zero config within one reconcile, via a query screenshot"

  @pending @plugin-tier @F4
  Scenario: F4 Double-dispatch — two racing orchestrators yield exactly one ship run
    Given the plugin tier for "github-orchestrator" is implemented
    When the acceptance criterion is exercised: "two orchestrators race to claim one new issue via assignee or the grinding label"
    Then evidence is captured: "exactly one ship run happens and the loser sees the existing claim, via two logs and one PR"

  @pending @plugin-tier @F5
  Scenario: F5 Panic isolation — a panicking plugin goes stale without taking others down
    Given the plugin tier for "claude" is implemented
    When the acceptance criterion is exercised: "the claude plugin panics"
    Then evidence is captured: "it shows stale while `_health` and the other plugins, including tmux, keep answering, via a `_health` screenshot taken after the induced panic"

  @pending @plugin-tier @F6
  Scenario: F6 — /health reports each plugin's last real event so a healthy plugin shows low lag
    Given the plugin tier for "github" is implemented
    When the acceptance criterion is exercised: "a plugin emits real events at a steady cadence"
    Then evidence is captured: "`/health` shows lagSeconds < 60 for it, derived from its last real event, via a `/health` screenshot"

  @pending @plugin-tier @F7
  Scenario: F7 Quota — GitHub API usage stays under budget at steady state
    Given the plugin tier for "github" is implemented
    When the acceptance criterion is exercised: "the org runs steady state with 20 repos"
    Then evidence is captured: "GitHub API calls stay under 500 per hour, via a rate-limit header log"

  @pending @plugin-tier @F8
  Scenario: F8 Stale peer — a stopped box is marked stale on its peers within 30 seconds
    Given the plugin tier for "peer" is implemented
    When the acceptance criterion is exercised: "a box stops"
    Then evidence is captured: "its peers, including tmux and claude data, show stale-since-T within 30s with no peer-of-peer rows, via a query screenshot and a grep"

  # --- AC Coverage Map ---
  # S1: "Issue-open to PR-open p95 <= 15min, zero human action" → Scenario: S1 — issue-open to PR-open stays under 15 minutes with zero human action
  # S2: "Cross-box free-slot query p95 < 1s warm" → Scenario: S2 — cross-box free-slot query answers warm in under 1 second
  # S3: "Webhook receipt to subscription push p95 < 1s" → Scenario: S3 — check-run webhook reaches a subscription push in under 1 second
  # S4: "One query joins github + claude by issue, source-tagged, stale marker on absence" → Scenario: S4 — one query joins github and claude rows by issue, marking absent sources
  # S5: "Template plugin compiles in, zero core diff" → Scenario: S5 — the template plugin compiles in and serves without touching core
  # S6: "/health reports last real event per plugin; stale + growing lag within 60s of stall" → Scenario: S6 — /health reports each plugin's last real event and marks a stalled one stale
  # F1: "Freshness — ingest-to-queryable p95 < 1s" → Scenario: F1 Freshness — github and claude events become queryable in under 1 second
  # F2: "Reconcile — dropped webhook healed, backfill < 60min" → Scenario: F2 Reconcile — a dropped webhook is healed by the next hourly reconcile
  # F3: "New repo appears with zero config within one reconcile" → Scenario: F3 New repo — a repo created after boot appears in the graph with zero config
  # F4: "Double-dispatch — exactly one ship run, loser sees claim" → Scenario: F4 Double-dispatch — two racing orchestrators yield exactly one ship run
  # F5: "Panic isolation — stale plugin, others keep answering" → Scenario: F5 Panic isolation — a panicking plugin goes stale without taking others down
  # F6: "/health reports last real event per plugin; healthy plugin lagSeconds < 60" → Scenario: F6 — /health reports each plugin's last real event so a healthy plugin shows low lag
  # F7: "Quota — GitHub API calls < 500/hour at 20 repos" → Scenario: F7 Quota — GitHub API usage stays under budget at steady state
  # F8: "Stale peer — stale since T within 30s, no peer-of-peer rows" → Scenario: F8 Stale peer — a stopped box is marked stale on its peers within 30 seconds
