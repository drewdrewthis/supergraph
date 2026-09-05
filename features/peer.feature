Feature: peer plugin — multi-box federation as a lazy mirror
  As the orchardist querying a fleet of boxes
  I want another box's data mirrored into a local cache, tagged by host and freshness
  So that a cross-box query is warm and sub-second, and a stopped box reads as stale, never empty

  # The peer plugin is a lazy pull-through mirror (PRD §6, §8: no federation router). It proxies named
  # ops to a remote's per-plugin executors (/plugins/<name>/graphql), caches the returned nodes in its
  # own SQLite keyed by their original @host key, and serves them back tagged host + lastSeenAt. It
  # tracks each remote's liveness with a WS-client subscription to the remote's existing base
  # `/graphql` pluginLag stream — needing NO core change and NO peer plugin on the remote. @local boots
  # TWO supergraph processes on loopback (the harness supports per-scenario binary+port); the remote is
  # a harness-only `fakeremote` source executor seeded from config, and for @AC-PEER-AUTH it is bound on
  # a non-loopback mesh address so core's mandatory-token rule is exercised. @live needs a real mesh
  # peer (a second box, or a real tmux+claude co-boot) and is @pending until run across boxes.

  # ---------- PRD ACs claimed by peer (cross-box, @local over two loopback processes) ----------

  @peer @local @S2
  Scenario: S2 — a warmed cross-box free-slot read answers from the local mirror under 1s over 20 samples
    Given a remote box seeded with free-slot rows for `boxB` is booted on loopback
    And a consumer box with a peer plugin pointed at `boxB` is booted
    And the consumer has warmed its mirror of `boxB` once
    When the consumer runs the warm `freeSlots` read against `boxB` 20 times
    Then every warm read is served from the local mirror with zero new calls to the remote
    And the p95 of the 20 warm-read latencies is under 1s
    And evidence is captured: "p95 warm cross-box free-slot read < 1s over 20 samples with zero remote calls, via a timing log and the remote's request log"

  @peer @local @F8
  Scenario: F8 — a stopped box is marked stale-since within 30 seconds with no peer-of-peer rows
    Given a remote box seeded with tmux and claude rows for `boxB`, plus a foreign `tmux:s1@boxC` row, is booted on loopback
    And a consumer box with a peer plugin pointed at `boxB` is booted
    And the consumer has warmed its mirror of `boxB` once
    When the remote box is stopped
    Then within 30s the consumer's `peers` shows `boxB` with a non-null `staleSince`
    And the consumer's warm read of `boxB` still returns its tmux and claude rows, flagged stale, never omitted
    And no mirrored row for `boxB` carries a hostId other than `boxB`
    And evidence is captured: "the remote reads stale-since-T within 30s, its rows still served flagged stale, no peer-of-peer rows, via a query result and a peer_nodes grep"

  # ---------- peer plugin ACs ----------

  @peer @local @AC-PEER-MIRROR
  Scenario: A proxied named op returns remote rows tagged by host and caches them
    Given a remote box seeded with `issue:o/r#5@boxB` is booted on loopback
    And a consumer box with a peer plugin pointed at `boxB` is booted
    When the consumer proxies the `issue` op to `boxB`
    Then the served node is tagged host `boxB` and carries a `lastSeenAt`
    And a row keyed `issue:o/r#5@boxB` is present in the consumer's `peer_nodes` store
    And the served node body equals the remote's seeded node body

  @peer @local @AC-PEER-REEMIT
  Scenario: Each mirrored node re-emits a local envelope preserving the origin host key
    Given a remote box seeded with `issue:o/r#5@boxB` is booted on loopback
    And a consumer box with a peer plugin pointed at `boxB` is booted
    When the consumer proxies the `issue` op to `boxB`
    Then a `peer.node.mirrored` envelope keyed `issue:o/r#5@boxB` is recorded by the consumer
    And the "peer" entry in the consumer's `/health` shows a non-null `lastEventAt`

  @peer @local @AC-PEER-LOOP
  Scenario: A foreign-host row from the remote is dropped and the remote's own peer plugin is never proxied
    Given a remote box seeded with a `tmux:s1@boxB` row and a foreign `tmux:s1@boxC` row is booted on loopback
    And a consumer box with a peer plugin pointed at `boxB` is booted
    And a direct query to the remote returns both the `boxB` row and the foreign `boxC` row
    When the consumer mirrors the remote's tmux data for `boxB`
    Then the consumer stores the `boxB` row and drops the foreign `boxC` row
    And no row keyed with hostId `boxC` is present in the consumer's `peer_nodes`
    And the remote box's own peer executor was never invoked
    And evidence is captured: "the remote offered a boxC row, the consumer dropped it and stored no peer-of-peer rows, via a direct query, a peer_nodes grep, and the remote's executor log"

  @peer @local @AC-PEER-UNKNOWN
  Scenario: A named op for an unknown host returns empty and makes no outbound hop
    Given a remote box seeded with `issue:o/r#5@boxB` is booted on loopback
    And a consumer box with a peer plugin pointed at `boxB` is booted
    When the consumer proxies the `issue` op to an unconfigured host `boxZ`
    Then the served result carries no nodes
    And the remote received no request for the unknown host

  @peer @local @AC-PEER-AUTH
  Scenario: A mesh-bound remote requires a bearer token; an absent or wrong token mirrors nothing
    Given a remote box seeded with `issue:o/r#5@boxB` is booted on a non-loopback mesh bind requiring token `s3cret` for `boxB`
    And a consumer box with a peer plugin pointed at `boxB` with no bearer token is booted
    When the consumer proxies the `issue` op to `boxB` with no Authorization
    Then the remote responds 401 and the consumer marks `boxB` unreachable with a non-null `staleSince`
    And no row for `boxB` is stored in the consumer's `peer_nodes`
    When the consumer is reconfigured with a wrong bearer token and proxies again
    Then the remote responds 401 and still nothing is mirrored for `boxB`
    When the consumer is reconfigured with the correct token `s3cret` and proxies again
    Then the proxy succeeds and at least one row for `boxB` is cached

  @peer @local @AC-PEER-RECONNECT
  Scenario: A peer that drops and returns reconnects with backoff and clears its stale mark
    Given a remote box seeded with `issue:o/r#5@boxB` is booted on loopback
    And a consumer box with a peer plugin pointed at `boxB` and a 2s max backoff is booted
    And the consumer has warmed its mirror of `boxB` once
    When the remote box is stopped
    Then within 30s the consumer's `peers` shows `boxB` with a non-null `staleSince`
    When the remote box is restarted on the same port
    Then within 4s the consumer's `peers` shows `boxB` with a null `staleSince` again
    And a fresh proxy to `boxB` refreshes the mirror

  @peer @local @AC-PEER-HEALTH
  Scenario: /health carries a peer entry with a null lastEventAt until a real mirror emit, no synthetic heartbeat
    Given a consumer box with a peer plugin pointed at `boxB`, whose remote is not running, is booted
    Then `GET /health` on the consumer returns HTTP 200 with a "peer" entry present
    And the consumer's "peer" entry has a null `lastEventAt`, because nothing has been mirrored and no synthetic heartbeat is emitted
    And within a short wait the consumer's `peers` shows `boxB` with a non-null `staleSince`, derived only from the failed connection attempt

  @peer @integration @AC-PEER-ZEROCORE
  Scenario: Adding the peer plugin touches zero files under core/ or server/
    Given the peer plugin exists under `plugins/peer/`, registered via `graph/plugins_import.go` and regenerated `graph/`
    When I check the tree
    Then `git diff --stat origin/main -- core server` reports no files changed
    And no file under core/ imports a plugin package

  @peer @integration @AC-PEER-LOC
  Scenario: The peer plugin stays within its prod LOC budget
    Given the peer plugin source under `plugins/peer/`
    When `make loc-peer` counts prod lines excluding tests
    Then the count is at most 690

  # ---------- Live cross-box proofs (honest @pending until run across boxes) ----------

  @peer @live @pending @S2
  Scenario: S2 — a real cross-box free-slot query is warm and sub-second across two boxes
    Given two real boxes are running on the mesh with a shared bearer token
    When a warmed cross-box `freeSlots` query runs 20 times against the peer box
    Then evidence is captured: "p95 cross-box free-slot query < 1s warm over 20 samples across two boxes, via paired timestamps"

  @peer @live @pending @F8
  Scenario: F8 — a real box stopped on the mesh reads stale-since within 30s with no peer-of-peer rows
    Given two real boxes are running on the mesh with a shared bearer token
    When one box is stopped
    Then evidence is captured: "the stopped box reads stale-since-T within 30s and no peer-of-peer rows are mirrored, via a query result and a grep"

  @peer @live @pending @AC-PEER-AUTH
  Scenario: AUTH — a real mesh peer rejects a wrong bearer with 401 and mirrors nothing
    Given two real boxes are running on the mesh, each with its own bearer token
    When the consumer proxies with a wrong bearer token
    Then evidence is captured: "the remote returns 401 and nothing is mirrored, via the response status and a peer_nodes grep"

  # --- AC Coverage Map ---
  # S2: "Cross-box free-slot query p95 < 1s warm" → Scenario: S2 — a warmed cross-box free-slot read answers from the local mirror under 1s over 20 samples (+ @live @pending real-box variant)
  # F8: "Stale peer — stale since T within 30s, no peer-of-peer rows" → Scenario: F8 — a stopped box is marked stale-since within 30 seconds with no peer-of-peer rows (+ @live @pending real-box variant)
  # AC-PEER-MIRROR: "Proxied op returns host-tagged rows, cached" → Scenario: A proxied named op returns remote rows tagged by host and caches them
  # AC-PEER-REEMIT: "Mirror re-emits, origin key preserved" → Scenario: Each mirrored node re-emits a local envelope preserving the origin host key
  # AC-PEER-LOOP: "Loop prevention, no peer-of-peer rows" → Scenario: A foreign-host row from the remote is dropped and the remote's own peer plugin is never proxied
  # AC-PEER-UNKNOWN: "Unknown host → empty, no outbound hop" → Scenario: A named op for an unknown host returns empty and makes no outbound hop
  # AC-PEER-AUTH: "Per-peer bearer, absent/wrong token mirrors nothing, 401" → Scenario: A mesh-bound remote requires a bearer token; an absent or wrong token mirrors nothing (+ @live @pending real-box variant)
  # AC-PEER-RECONNECT: "Backoff reconnect clears stale within 2x backoffMax" → Scenario: A peer that drops and returns reconnects with backoff and clears its stale mark
  # AC-PEER-HEALTH: "/health peer entry, null lastEventAt until real emit, no heartbeat" → Scenario: /health carries a peer entry with a null lastEventAt until a real mirror emit, no synthetic heartbeat
  # AC-PEER-ZEROCORE: "Zero core/server edit" → Scenario: Adding the peer plugin touches zero files under core/ or server/
  # AC-PEER-LOC: "Prod LOC <= 690" → Scenario: The peer plugin stays within its prod LOC budget
