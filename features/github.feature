Feature: GitHub plugin — event-invalidated caching proxy
  As the orchardist and ship worker
  I want github issues, PRs, and check runs served from a lazy cache that webhooks purge by object id
  So that reads are fresh and sub-second, stay under the API budget, and never miss an event

  # The github plugin is a read-through caching proxy (PAT only, no GitHub App). Nodes are keyed by
  # object id (issue:owner/repo#N, pr:owner/repo#N, checkRun:owner/repo/<id>, repo:owner/repo, ...);
  # a webhook purges exactly the entries that touch the changed object (tag purge = keyed delete);
  # ETag/304 makes unchanged reads free; immutable nodes are pinned. @local runs against a fake GitHub
  # httptest server (GraphQL + REST hooks/deliveries + /notifications 304 + ETag echo + settable
  # rateLimit) plus synthetic signed webhook POSTs and a fake `gh webhook forward` stub. @live needs
  # env GITHUB_TOKEN + GITHUB_ORG and is @pending until run against live GitHub.

  # ---------- PRD ACs (verbatim; github scope) ----------

  @github @local @S3
  Scenario: S3 — check-run webhook to subscription push p95 under 1 second over 20 samples
    Given a supergraph server started with the github plugin and data dir <tmp>
    And a websocket subscription to `checkRunUpdated` on `127.0.0.1:7788/graphql`
    When 20 correctly-signed check_run webhooks are POSTed to `/plugins/github/webhook`
    Then the p95 of receipt-to-subscription-push over the 20 samples is under 1s
    And evidence is captured: "p95 webhook-receipt to subscription-push < 1s over 20 samples, via paired journald lines"

  @github @local @AC-GH-ISSUE-SUB
  Scenario: An issues webhook pushes on the issueUpdated subscription within the S3 bound
    Given a supergraph server started with the github plugin and data dir <tmp>
    And a websocket subscription to `issueUpdated` on `127.0.0.1:7788/graphql`
    When a correctly-signed `issues` `labeled` webhook for `o/r#5` is received
    Then an `issueUpdated` push for `issue:o/r#5` is received within 1s of the webhook receipt

  @github @local @F1
  Scenario: F1 — github event ingest-to-queryable p95 under 1 second over 20 samples
    Given a supergraph server started with the github plugin and data dir <tmp>
    When 20 correctly-signed issues "opened" webhooks are POSTed to `/plugins/github/webhook`
    Then each issue is queryable and the p95 of emit-to-queryable over the 20 samples is under 1s
    And evidence is captured: "p95 ingest-to-queryable < 1s over 20 samples, via paired log timestamps"

  @github @local @F2
  Scenario: F2 — a dropped webhook is healed within one reconcile window
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server has an issue that no webhook was delivered for
    When the since-cursor reconcile runs once
    Then a query for that issue returns it, healed without a webhook
    And evidence is captured: "the dropped event is present after the next reconcile window, via a log"

  @github @local @F3
  Scenario: F3 — a repo added to the account is discovered by reconcile with zero config
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub `/user/repos` listing gains a new repo after boot
    When the reconcile loop runs once
    Then a query for open issues covers the new repo with no config change
    And a hook is created for the new repo
    And evidence is captured: "the new repo appears in the graph with zero config within one reconcile, via a log"

  # ---------- Caching-proxy plugin ACs ----------

  @github @local @AC-GH-CACHE-HIT
  Scenario: A cached node is served with no upstream call and the stored etag
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is already in the store, fresh, with etag `W/"abc"`
    When `issue` is queried for `o/r#5`
    Then the complete stored node is served
    And the served node's etag equals the stored etag `W/"abc"`
    And the fake GitHub server records zero requests during the query

  @github @local @AC-GH-STALE
  Scenario: A node mutated upstream with no event arriving is served stale until reconcile
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is in the store, fresh, with title "old"
    And the fake GitHub server's `o/r#5` is changed to title "new" with no webhook, notification, or reconcile
    When `issue` is queried for `o/r#5`
    Then the stale cached node with title "old" is served, by design
    When the since-cursor reconcile runs once
    Then `issue:o/r#5` is purged and the next query returns title "new"
    And evidence is captured: "worst-case staleness is bounded by the reconcile interval, via a log"

  @github @local @AC-GH-COLDSTART
  Scenario: Cold start has no baseline — first read of each named op is one read-through
    Given a supergraph server started with the github plugin and an empty data dir <tmp>
    Then no bulk backfill runs
    When the first read of each named op runs against the fake GitHub server
    Then each op completes in under 2 seconds
    And each op populates exactly the keys it declares in its `# keys`/`# scope` directive and nothing more
    And evidence is captured: "cold cache serves each read via one read-through fetch, no backfill, via a log and a timer"

  @github @local @AC-GH-ETAG-304
  Scenario: A conditional read-through that 304s costs zero quota
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#5` is in the store with a stored etag but marked expired
    And the fake GitHub server returns 304 for `If-None-Match` matching that etag
    When `issue` is queried for `o/r#5`
    Then the read-through sends `If-None-Match` with the stored etag
    And the fake server returns 304 and the stored node is served with its fetched_at bumped
    And no GraphQL points or REST quota are spent on the body

  @github @local @AC-GH-PURGE-TAG
  Scenario: A webhook purges exactly the entries that touch the changed object
    Given a supergraph server started with the github plugin and data dir <tmp>
    And these entries are cached: `issue:o/r#5`, an `openIssues(o/r)` list result, and `issue:o/r#6`
    When a correctly-signed `issue` `edited` webhook for `o/r#5` is received
    Then `issue:o/r#5` is deleted and the `openIssues(o/r)` list result is deleted
    And `issue:o/r#6` is still cached
    And a `github.node.purged` envelope is emitted with key `issue:o/r#5`

  @github @local @AC-GH-PIN
  Scenario: An immutable node is pinned and never refetched
    Given a supergraph server started with the github plugin and data dir <tmp>
    And a merged PR `pr:o/r#9` closed more than the pin grace window ago is in the store
    When `pr:o/r#9` is queried twice after its TTL would have expired
    Then it is served from the store both times
    And no conditional or full fetch is made to the fake GitHub server

  @github @local @AC-GH-SINGLEFLIGHT
  Scenario: Concurrent misses on one key coalesce into a single upstream fetch
    Given a supergraph server started with the github plugin and data dir <tmp>
    And issue `issue:o/r#7` is not in the store
    When 10 concurrent queries for `issue:o/r#7` arrive
    Then the fake GitHub server receives exactly one fetch for it
    And all 10 queries return the same stored node

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
  Scenario: The gh webhook forward child is restarted with backoff and redelivery
    Given a github plugin configured for `forward` ingress with a fake `gh webhook forward` stub
    When the plugin starts and the stub delivers a signed webhook to the handler
    Then the webhook is ingested
    When the stub child exits
    Then the plugin restarts it with backoff and runs redelivery from the last-seen delivery id
    And a delivery missed while it was down is replayed via `/attempts` exactly once

  @github @local @AC-GH-NOTIFY-304
  Scenario: The notifications poll only runs behind the flag and a 304 costs zero quota
    Given a supergraph server started with the github plugin and data dir <tmp>
    And `notifications` is false by default
    Then the notifications poll does not run
    When the server is restarted with `notifications = true`
    And the fake `/notifications` returns 304 for the stored `If-Modified-Since`
    And the notifications poll runs
    Then no node fetch is made and no quota is spent
    When `/notifications` next returns 200 with a changed thread
    Then that change feeds the cursor and store

  @github @local @AC-GH-FLOOR
  Scenario: Near the GraphQL points floor the plugin pauses to resetAt and resumes
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub GraphQL returns `rateLimit` remaining near the floor with a near-future `resetAt`
    When a read-through or reconcile hits the floor
    Then the plugin pauses until `resetAt` rather than erroring
    And it does not panic or exit and `/health` shows no forced-stale for github
    When the fake server restores points after `resetAt`
    Then the paused work resumes and completes

  @github @local @AC-GH-RATELOG
  Scenario: Rate-limit fields are logged on every GitHub response
    Given a supergraph server started with the github plugin and data dir <tmp>
    When the plugin makes a request to the fake GitHub server
    Then the log records the REST `x-ratelimit-*` headers and the GraphQL `rateLimit{remaining,resetAt}`

  @github @local @AC-GH-NAMEDOP-KEYS
  Scenario: A named op scopes its result to object ids so a purge of one evicts the list
    Given a supergraph server started with the github plugin and data dir <tmp>
    When I run `supergraph query --op openIssues --var owner=o --var repo=r`
    Then the `plugins/github/queries/openIssues.graphql` op runs and its result is tagged with scope `repo:o/r`
    And each returned issue node is stored under its own key
    When a correctly-signed `issue` `closed` webhook for `o/r#5` is received
    Then the `openIssues(o/r)` list result is evicted by the covering scope
    And I run `supergraph schema Issue` and the `Issue` type definition is printed

  @github @local @AC-GH-CURSOR
  Scenario: The per-repo since cursor persists across a restart and drives the next fetch
    Given a supergraph server started with the github plugin and data dir <tmp>
    And one reconcile has advanced the `since` cursor for a repo to time T
    When the server is stopped and restarted on the same data dir <tmp>
    Then the `since` cursor for that repo is still T after the restart
    And the next reconcile's fetch to the fake GitHub server carries `since=` equal to T minus 60s

  @github @local @AC-GH-RECONCILE-SHAPE
  Scenario: Reconcile revalidation keeps the canonical GraphQL node shape
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server has a rich issue `o/r#5` with labels, an assignee, and updatedAt
    And the `openIssues` op has been warmed once for `o/r`
    When the since-cursor reconcile runs once
    Then a query for issue `o/r#5` still shows its labels, assignee, and updatedAt

  @github @local @AC-GH-RECONCILE-NOEVENT
  Scenario: Reconcile revalidation of an unchanged node emits no update event
    Given a supergraph server started with the github plugin and data dir <tmp>
    And the fake GitHub server has a rich issue `o/r#5` with labels, an assignee, and updatedAt
    And the `openIssues` op has been warmed once for `o/r`
    When the since-cursor reconcile runs once
    Then no `github.node.updated` event was emitted for `issue:o/r#5`

  @github @local @AC-GH-LOC @AC-GHPR-LOC
  Scenario: Production LOC for the github plugin stays within the 1810-line budget
    Given the github plugin source under `plugins/github`
    When `make loc-github` counts non-comment, non-blank prod lines excluding tests and `internal/fakegh`
    Then the count is 1810 or fewer
    And evidence is captured: "plugins/github prod LOC <= 1810, via make loc-github output"

  @github @local @AC-GH-ZEROCORE
  Scenario: The github plugin compiles in without touching core
    Given the github plugin package and its blank import in graph/plugins_import.go
    When `git diff --stat core/` is run
    Then it reports 0 files changed

  # ---------- Live proofs (honest @pending) ----------

  @github @live @pending @F2
  Scenario: F2 — a real dropped delivery is healed by redelivery or reconcile
    Given the github plugin is running against the live account
    And a real webhook delivery is dropped
    When boot redelivery or the next since-cursor reconcile runs
    Then the dropped object is present in the graph
    And evidence is captured: "the dropped event is healed within one window, via a log"

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

  @github @live @pending @AC-GH-NOTIFY-304
  Scenario: Live /notifications honors If-Modified-Since and X-Poll-Interval
    Given the github plugin polling live `/notifications` with `notifications = true`
    When there are no changes since the stored `If-Modified-Since`
    Then GitHub returns 304 at the advertised `X-Poll-Interval` and no quota is spent

  @github @live @AC-GH-RATELOG
  Scenario: Live rate-limit fields are logged
    Given the github plugin running against live GitHub
    When it makes a live GraphQL request
    Then the log records the live `rateLimit{remaining,resetAt}`
