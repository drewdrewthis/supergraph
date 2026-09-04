Feature: GitHub plugin
  As the orchardist and ship worker
  I want github issues, PRs, and check runs ingested with sub-second freshness and no missed events
  So that a new issue reaches a worker and a check run reaches its subscriber without hitting rate limits

  # @local scenarios run against a fake GitHub httptest server + synthetic signed webhook POSTs.
  # @live scenarios need env GITHUB_TOKEN and GITHUB_ORG=drewdrewthis AND the owner-created GitHub
  # App; they are @pending until the App exists. Local and @live tags mirror the EDR split exactly.

  @github @local @S3
  Scenario: S3 — check-run webhook to subscription push p95 under 1 second over 20 samples
    Given a supergraph server started with the github plugin and data dir <tmp>
    And a websocket subscription to `checkRunUpdated` on `127.0.0.1:7788/graphql`
    When 20 correctly-signed check_run webhooks are POSTed to `/plugins/github/webhook`
    Then the p95 of receipt-to-subscription-push over the 20 samples is under 1s
    And evidence is captured: "p95 webhook-receipt to subscription-push < 1s over 20 samples, via paired journald lines"

  @github @local @F1
  Scenario: F1 — github event ingest-to-queryable p95 under 1 second over 20 samples
    Given a supergraph server started with the github plugin and data dir <tmp>
    When 20 correctly-signed issues "opened" webhooks are POSTed to `/plugins/github/webhook`
    Then each issue is queryable via `githubIssue` and the p95 of emit-to-queryable over the 20 samples is under 1s
    And evidence is captured: "p95 ingest-to-queryable < 1s over 20 samples, via paired log timestamps"

  @github @local @F2
  Scenario: F2 — a dropped webhook is healed by the next reconcile
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server has an issue that no webhook was delivered for
    When the reconcile loop runs once
    Then a query of `githubIssues` includes the dropped issue
    And evidence is captured: "the dropped event is present after reconcile, via a log"

  @github @local @F3
  Scenario: F3 — a repo added to the account is discovered by reconcile with zero config
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server's repo listing gains a new repo after boot
    When the reconcile loop runs once
    Then a query of `githubIssues` covers the new repo with no config change
    And evidence is captured: "the new repo appears in the graph with zero config within one reconcile, via a log"

  @github @local @AC-GH-RATELIMIT
  Scenario: On rate-limit exhaustion the loop sleeps to reset and resumes without hard-failing
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server returns `X-RateLimit-Remaining: 0` with a near-future `X-RateLimit-Reset`
    When the reconcile loop hits the exhausted limit
    Then the plugin sleeps until the reset time rather than erroring
    And the plugin does not panic or exit and `/health` shows no forced-stale for github
    When the fake server restores remaining quota after the reset
    Then the reconcile loop resumes and ingests the pending issues

  @github @local @AC-GH-OVERLAP
  Scenario: An event edited within the since-60s overlap window is still ingested
    Given a supergraph server started with the github plugin and data dir <tmp>
    And a reconcile has advanced the `since` cursor for a repo to time T
    And the fake GitHub server has an issue edited within 60s before T
    When the reconcile loop runs again with the overlap window applied
    Then that issue is ingested and not skipped

  @github @local @AC-GH-HMAC
  Scenario: A webhook with a bad signature is rejected with 401 and emits nothing
    Given a supergraph server started with the github plugin and data dir <tmp>
    When a webhook with an invalid `X-Hub-Signature-256` is POSTed to `/plugins/github/webhook`
    Then the response status is 401
    And no github event is emitted
    When a correctly-signed webhook is POSTed to `/plugins/github/webhook`
    Then the response status is 200
    And exactly one github event is emitted

  @github @local @AC-GH-CURSOR
  Scenario: The per-repo since cursor persists across a restart and drives the next request
    Given a supergraph server started with the github plugin and data dir <tmp>
    And one reconcile has advanced the `since` cursor for a repo to time T
    When the server is stopped and restarted on the same data dir <tmp>
    Then the `since` cursor for that repo is still T after the restart
    And the next reconcile's issues request to the fake GitHub server carries `since=` equal to T

  @github @local @AC-GH-RATELOG
  Scenario: Rate-limit headers are logged on every GitHub response
    Given a supergraph server started with the github plugin and data dir <tmp>
    When the reconcile loop makes a request to the fake GitHub server
    Then the log records `x-ratelimit-limit`, `x-ratelimit-remaining`, and `x-ratelimit-reset`

  @github @local @AC-GH-PRFILTER
  Scenario: An issues-endpoint item that is a pull request is not emitted as an issue
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server returns one issue and one pull-request item on the issues endpoint
    When the reconcile loop runs once
    Then a `pr.opened` event is emitted for the pull-request item
    And no `issue.opened` event is emitted for the pull-request item

  @github @local @AC-GH-REDELIVER-APP
  Scenario: In app mode a failed delivery is replayed exactly once on boot
    Given a github plugin in `app` mode against the fake GitHub server
    And the fake `/app/hook/deliveries` has one failed delivery past the last-seen delivery id
    When the github plugin boots and runs its redelivery step
    Then the delivery is replayed via `/attempts` and exactly one github event is emitted
    And a second boot does not re-emit it, deduped by `X-GitHub-Delivery`
    And the `webhook:lastDeliveryId` cursor advances past it

  @github @local @AC-GH-REDELIVER-PAT
  Scenario: In pat mode redelivery is skipped with a warn log
    Given a github plugin in `pat` mode against the fake GitHub server
    When the github plugin boots
    Then no `/app/hook/deliveries` request is made
    And the log records a warn that redelivery is skipped in pat mode

  @github @local @AC-GH-ZEROCORE
  Scenario: The github plugin compiles in without touching core
    Given the github plugin package and its blank import in graph/plugins_import.go
    When `git diff --stat core/` is run
    Then it reports 0 files changed

  @github @live @pending @F2
  Scenario: F2 — org backfill from an empty db completes under 60 minutes
    Given the github plugin started against the live account with an empty data dir <tmp>
    When the boot reconcile runs to completion
    Then the backfill completes in under 60 minutes
    And evidence is captured: "backfill completes < 60 min, via a timer"

  @github @live @pending @F3
  Scenario: F3 — a repo created on the account after boot appears with zero config within one reconcile
    Given the github plugin is running against the live account
    When a new repo is created and the next reconcile runs
    Then a query of `githubIssues` covers the new repo with no config change
    And evidence is captured: "the repo appears in the graph with zero config within one reconcile, via a query screenshot"

  @github @live @pending @F7
  Scenario: F7 — GitHub API usage stays under budget at steady state
    Given the github plugin is running against the live account with 20 repos
    When it runs at steady state for one hour
    Then fewer than 500 GitHub API calls are made in that hour
    And evidence is captured: "GitHub API calls stay under 500 per hour, via a rate-limit header log"

  @github @live @pending @AC-GH-RATELOG
  Scenario: Live rate-limit headers are logged
    Given the github plugin is running against the live account
    When it makes a live GitHub request
    Then the log records the live `x-ratelimit-*` headers

  @github @live @pending @AC-GH-REDELIVER-APP
  Scenario: Live App redelivery replays a real missed delivery on boot
    Given the github plugin in `app` mode against the live GitHub App
    And a real delivery was missed while the ingress box was down
    When the github plugin boots and runs its redelivery step
    Then the missed delivery is replayed and ingested exactly once
