Feature: peer plugin — multi-box federation as a lazy mirror
  As the orchardist querying a fleet of boxes
  I want another box's data mirrored into a local cache, tagged by host and freshness
  So that a cross-box query is warm and sub-second, and a stopped box reads as stale, never empty

  # The peer plugin is a lazy pull-through mirror (PRD §6, §8: no federation router). It proxies named
  # ops to a remote's per-plugin executors (/plugins/<name>/graphql), caches the returned nodes in its
  # own SQLite keyed by their original @hostId key, and serves them back tagged host + lastSeenAt. It
  # tracks each remote's liveness with a WS-client subscription to the remote's existing base
  # `/graphql` pluginLag stream — needing NO core change and NO peer plugin on the remote. @local boots
  # TWO supergraph processes on loopback (the harness supports per-scenario binary+port); the remote is
  # bound at `:PORT` for the auth scenario so core's non-loopback token rule is exercised. @live needs a
  # real mesh peer and is @pending until run across boxes.

  # ---------- PRD ACs claimed by peer (verbatim scope) ----------

  @peer @local @S2
  Scenario: S2 — cross-box free-slot query answers warm in under 1 second over 20 samples
    Given a remote supergraph process is booted on loopback with a seeded free-slot dataset
    And a consumer supergraph process is booted with a peer plugin pointed at the remote
    And the consumer has warmed its mirror of the remote's slots once
    When the consumer runs the `freeSlots` named op against the remote's hostId 20 times
    Then every result is served from the local mirror with zero calls to the remote
    And the p95 of the 20 query latencies is under 1s
    And evidence is captured: "p95 cross-box free-slot query < 1s warm over 20 samples, via a timing log"

  @peer @local @F8
  Scenario: F8 — a stopped box is marked stale-since within 30 seconds with no peer-of-peer rows
    Given a remote supergraph process is booted on loopback with seeded tmux and claude rows
    And a consumer supergraph process is booted with a peer plugin pointed at the remote
    And the consumer has warmed its mirror of the remote once
    When the remote process is stopped
    Then within 30s the consumer's `peers` shows the remote with a non-null `staleSince`
    And the remote's mirrored tmux and claude rows are still queryable, flagged stale, never omitted
    And no mirrored row carries a hostId other than the remote's own
    And evidence is captured: "the remote reads stale-since-T within 30s, no peer-of-peer rows, via a query result and a grep"

  # ---------- peer plugin ACs ----------

  @peer @local @AC-PEER-MIRROR
  Scenario: A proxied named op returns remote rows tagged by host and caches them
    Given a remote supergraph process is booted on loopback with a seeded issue `issue:o/r#5@boxB`
    And a consumer supergraph process is booted with a peer plugin pointed at the remote as `boxB`
    When the consumer proxies the `issue` named op for `o/r#5` to `boxB`
    Then the returned node is tagged hostId `boxB` and carries a `lastSeenAt`
    And a row keyed `issue:o/r#5@boxB` is present in the consumer's `peer_nodes` store
    And the served node equals the remote's node body

  @peer @local @AC-PEER-REEMIT
  Scenario: Each mirrored node re-emits a local envelope preserving the origin host key
    Given a remote supergraph process is booted on loopback with a seeded issue `issue:o/r#5@boxB`
    And a consumer supergraph process is booted with a peer plugin pointed at the remote as `boxB`
    When the consumer mirrors `issue:o/r#5@boxB`
    Then a `peer.node.mirrored` envelope is emitted with key `issue:o/r#5@boxB`
    And the "peer" entry in the consumer's `/health` shows its `lastEventAt` advanced to that mirror

  @peer @local @AC-PEER-LOOP
  Scenario: A foreign-host row from the remote is dropped and the remote's own peer plugin is never proxied
    Given a remote supergraph process is booted on loopback that itself holds a mirrored `tmux:s1@boxC` row
    And a consumer supergraph process is booted with a peer plugin pointed at the remote as `boxB`
    When the consumer mirrors the remote's tmux data
    Then the consumer stores only rows whose key hostId is `boxB`
    And no row keyed with hostId `boxC` is stored
    And the consumer never issues a request to the remote's `/plugins/peer/graphql`
    And evidence is captured: "no peer-of-peer rows stored, via a peer_nodes grep and a request log"

  @peer @local @AC-PEER-AUTH
  Scenario: A mesh-bound remote requires a bearer token; the wrong token mirrors nothing
    Given a remote supergraph process is booted bound at `:PORT` with a `[tokens]` entry `boxB = "s3cret"`
    And a consumer supergraph process is booted with a peer plugin pointed at the remote as `boxB`
    When the peer plugin subscribes and proxies with a wrong bearer token
    Then the remote responds 401 and the consumer marks `boxB` unreachable with a non-null `staleSince`
    And no row is stored in `peer_nodes` for `boxB`
    When the peer plugin re-subscribes with the correct token `s3cret`
    Then the subscription succeeds and a subsequent proxy caches at least one row for `boxB`

  @peer @local @AC-PEER-RECONNECT
  Scenario: A peer that drops and returns reconnects with backoff and clears its stale mark
    Given a remote supergraph process is booted on loopback and mirrored once by the consumer
    When the remote process is stopped
    Then within 30s the consumer marks `boxB` with a non-null `staleSince`
    When the remote process is restarted on the same port
    Then the consumer's backoff loop reconnects to the remote's `pluginLag` stream
    And the consumer's `peers` shows `boxB` with a null `staleSince` again
    And the next proxied query refreshes the mirror for `boxB`

  @peer @local @AC-PEER-HEALTH
  Scenario: /health carries a peer entry that goes stale when every peer is unreachable
    Given a consumer supergraph process is booted with a peer plugin pointed at one remote as `boxB`
    And the remote is not running
    Then `GET /health` returns HTTP 200 with a "peer" entry present
    And within the lag threshold the "peer" entry state becomes "stale" with a growing `lagSeconds`
    And the state is derived only from the peer plugin's real emit path, with no synthetic heartbeat

  @peer @integration @AC-PEER-ZEROCORE
  Scenario: Adding the peer plugin touches zero files under core/
    Given the peer plugin exists under `plugins/peer/`, registered via `graph/plugins_import.go` and regenerated `graph/`
    When I check the tree
    Then `git diff --stat core/` reports 0 files changed
    And no file under core/ imports a plugin package

  @peer @integration @AC-PEER-LOC
  Scenario: The peer plugin stays within its prod LOC budget
    Given the peer plugin source under `plugins/peer/`
    When `make loc-peer` counts prod lines excluding tests and internal/fakepeer
    Then the count is at most 700

  # --- AC Coverage Map ---
  # S2: "Cross-box free-slot query p95 < 1s warm" → Scenario: S2 — cross-box free-slot query answers warm in under 1 second over 20 samples
  # F8: "Stale peer — stale since T within 30s, no peer-of-peer rows" → Scenario: F8 — a stopped box is marked stale-since within 30 seconds with no peer-of-peer rows
  # AC-PEER-MIRROR: "Proxied op returns host-tagged rows, cached" → Scenario: A proxied named op returns remote rows tagged by host and caches them
  # AC-PEER-REEMIT: "Mirror re-emits, origin key preserved" → Scenario: Each mirrored node re-emits a local envelope preserving the origin host key
  # AC-PEER-LOOP: "Loop prevention, no peer-of-peer rows" → Scenario: A foreign-host row from the remote is dropped and the remote's own peer plugin is never proxied
  # AC-PEER-AUTH: "Per-peer bearer, 401 mirrors nothing" → Scenario: A mesh-bound remote requires a bearer token; the wrong token mirrors nothing
  # AC-PEER-RECONNECT: "Backoff reconnect clears stale" → Scenario: A peer that drops and returns reconnects with backoff and clears its stale mark
  # AC-PEER-HEALTH: "/health peer entry goes stale, no heartbeat" → Scenario: /health carries a peer entry that goes stale when every peer is unreachable
  # AC-PEER-ZEROCORE: "Zero core edit" → Scenario: Adding the peer plugin touches zero files under core/
  # AC-PEER-LOC: "Prod LOC <= 700" → Scenario: The peer plugin stays within its prod LOC budget
