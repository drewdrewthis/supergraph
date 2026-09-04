Feature: GitHub plugin
  As the orchardist and ship worker
  I want github issues, PRs, and check runs ingested via webhooks with sub-second freshness and no missed events
  So that a new issue reaches a worker and a check run reaches its subscriber without hitting rate limits

  # Ingest is webhook-push (PAT only, no GitHub App). @local runs against a fake GitHub httptest
  # server (GraphQL + REST hooks/deliveries + /notifications 304 + settable rateLimit) plus synthetic
  # signed webhook POSTs and a fake `gh webhook forward` stub. @live needs env GITHUB_TOKEN +
  # GITHUB_ORG and is @pending until run against live GitHub. Tags mirror the EDR split exactly.

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
    Then each issue is queryable and the p95 of emit-to-queryable over the 20 samples is under 1s
    And evidence is captured: "p95 ingest-to-queryable < 1s over 20 samples, via paired log timestamps"

  @github @local @F2
  Scenario: F2 — a dropped webhook is healed by the next reconcile
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server has an issue that no webhook was delivered for
    When the reconcile loop runs once
    Then a query for open issues includes the dropped issue
    And evidence is captured: "the dropped event is present after reconcile, via a log"

  @github @local @F2
  Scenario: F2 — the shallow baseline backfill from empty is measured against one hour
    Given a supergraph server started with the github plugin and an empty data dir <tmp>
    And the fake GitHub server has 20 repos of open issues, PRs, checks, reviews, comments, labels, and refs
    When the boot baseline backfill runs to completion
    Then the elapsed backfill time is recorded and is under 60 minutes
    And evidence is captured: "baseline completes < 60 min, via a timer"

  @github @local @F3
  Scenario: F3 — a repo added to the account is discovered by reconcile with zero config
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub `/user/repos` listing gains a new repo after boot
    When the reconcile loop runs once
    Then a query for open issues covers the new repo with no config change
    And evidence is captured: "the new repo appears in the graph with zero config within one reconcile, via a log"

  @github @local @AC-GH-HMAC
  Scenario: A webhook with a bad signature is rejected with 401 and emits nothing
    Given a supergraph server started with the github plugin and data dir <tmp>
    When a webhook with an invalid `X-Hub-Signature-256` is POSTed to `/plugins/github/webhook`
    Then the response status is 401
    And no github event is emitted
    When a correctly-signed webhook is POSTed to `/plugins/github/webhook`
    Then the response status is 200
    And exactly one github event is emitted

  @github @local @AC-GH-FORWARD
  Scenario: The gh webhook forward subprocess is supervised and restarted
    Given a github plugin configured for `forward` ingress with a fake `gh webhook forward` stub
    When the plugin starts and the stub delivers a signed webhook to the handler
    Then the webhook is ingested
    When the stub subprocess exits
    Then the plugin restarts it and a subsequent delivery is still ingested

  @github @local @AC-GH-REDELIVER
  Scenario: A failed repo-hook delivery is replayed exactly once on boot
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake `/repos/{owner}/{repo}/hooks/{id}/deliveries` has one failed delivery past the stored id
    When the plugin boots and runs its redelivery step
    Then the delivery is replayed via `/attempts` and exactly one github event is emitted
    And a second boot does not re-emit it, deduped by `X-GitHub-Delivery`

  @github @local @AC-GH-NOTIFY-304
  Scenario: A 304 from /notifications costs zero quota and triggers no re-fetch
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake `/notifications` returns 304 for the stored `If-Modified-Since`
    When the notifications poll runs
    Then no GraphQL or node fetch is made and no quota is spent
    When `/notifications` next returns 200 with a changed thread
    Then that change feeds the cursor and store

  @github @local @AC-GH-RATELIMIT-FLOOR
  Scenario: Near the GraphQL points floor the backfill pauses to resetAt and resumes
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub GraphQL returns `rateLimit` remaining near the floor with a near-future `resetAt`
    When the baseline backfill hits the floor
    Then the plugin pauses until `resetAt` rather than erroring
    And the plugin does not panic or exit and `/health` shows no forced-stale for github
    When the fake server restores points after `resetAt`
    Then the backfill resumes and completes

  @github @local @AC-GH-OVERLAP
  Scenario: An event edited within the since-60s overlap window is still ingested
    Given a supergraph server started with the github plugin and data dir <tmp>
    And a reconcile has advanced the `since` cursor for a repo to time T
    And the fake GitHub server has an issue edited within 60s before T
    When the reconcile loop runs again with the overlap window applied
    Then that issue is ingested and not skipped

  @github @local @AC-GH-CURSOR
  Scenario: The per-repo since cursor persists across a restart and drives the next fetch
    Given a supergraph server started with the github plugin and data dir <tmp>
    And one reconcile has advanced the `since` cursor for a repo to time T
    When the server is stopped and restarted on the same data dir <tmp>
    Then the `since` cursor for that repo is still T after the restart
    And the next reconcile's fetch to the fake GitHub server carries `since=` equal to T

  @github @local @AC-GH-JSONNODE
  Scenario: A node not in store is read-through fetched, stored, and served complete
    Given a supergraph server started with the github plugin and data dir <tmp>
    And an issue node that is not yet in the store
    When that issue is queried
    Then the plugin makes one anchored fetch, stores the node, and serves the complete JSON node
    And a second query for it makes no further fetch

  @github @local @AC-GH-SCHEMA-SUBSET
  Scenario: The served github schema is a strict subset of the vendored upstream SDL
    Given the vendored GitHub SDL with the Mutation root stripped
    When the served github schema is compared to the upstream SDL
    Then every github type and field exists in upstream with no `Github` prefix
    And the only additions are via `extend type` adding `origin` and `fetchedAt`

  @github @local @AC-GH-NAMEDOP
  Scenario: Named operations run by name and a type can be printed
    Given a supergraph server started with the github plugin and data dir <tmp>
    When I run `supergraph query --op openIssues --var repo=drewdrewthis/supergraph`
    Then the named `plugins/github/queries/openIssues.graphql` operation runs and returns issues
    When I run `supergraph schema Issue`
    Then the `Issue` type definition is printed

  @github @local @AC-GH-RATELOG
  Scenario: Rate-limit fields are logged on every GitHub response
    Given a supergraph server started with the github plugin and data dir <tmp>
    When the reconcile loop makes a request to the fake GitHub server
    Then the log records the REST `x-ratelimit-*` headers and the GraphQL `rateLimit{remaining,resetAt}`

  @github @local @AC-GH-ZEROCORE
  Scenario: The github plugin compiles in without touching core
    Given the github plugin package and its blank import in graph/plugins_import.go
    When `git diff --stat core/` is run
    Then it reports 0 files changed

  @github @live @pending @F2
  Scenario: F2 — live baseline backfill from an empty db completes under 60 minutes
    Given the github plugin started against the live account with an empty data dir <tmp>
    When the boot baseline backfill runs to completion
    Then the backfill completes in under 60 minutes
    And evidence is captured: "backfill completes < 60 min, via a timer"

  @github @live @pending @F3
  Scenario: F3 — a repo created on the account after boot appears within one reconcile
    Given the github plugin is running against the live account
    When a new repo is created and the next reconcile runs
    Then a query for open issues covers the new repo with no config change
    And evidence is captured: "the repo appears in the graph with zero config within one reconcile, via a query screenshot"

  @github @live @pending @F7
  Scenario: F7 — GitHub API usage stays under budget at steady state
    Given the github plugin is running against the live account with 20 repos
    When it runs at steady state for one hour
    Then GraphQL points and REST calls stay under the 500-per-hour budget
    And evidence is captured: "GitHub API usage stays under 500 per hour, via a rate-limit log"

  @github @live @pending @AC-GH-FORWARD
  Scenario: Live gh webhook forward streams real deliveries to the handler
    Given the github plugin in `forward` ingress against live GitHub
    When a real event fires on a watched repo
    Then `gh webhook forward` streams it over the outbound websocket to the handler and it is ingested

  @github @live @pending @AC-GH-REDELIVER
  Scenario: Live repo-hook redelivery replays a real missed delivery on boot
    Given the github plugin against live GitHub with a real missed delivery
    When the plugin boots and runs its redelivery step
    Then the missed delivery is replayed and ingested exactly once

  @github @live @pending @AC-GH-NOTIFY-304
  Scenario: Live /notifications honors If-Modified-Since and X-Poll-Interval
    Given the github plugin polling live `/notifications`
    When there are no changes since the stored `If-Modified-Since`
    Then GitHub returns 304 at the advertised `X-Poll-Interval` and no quota is spent

  @github @live @pending @AC-GH-RATELOG
  Scenario: Live rate-limit fields are logged
    Given the github plugin running against live GitHub
    When it makes a live GraphQL request
    Then the log records the live `rateLimit{remaining,resetAt}`
