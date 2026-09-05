Feature: tmux plugin — server-wide control-mode watcher over local SQLite
  As the orchardist and the cross-source join
  I want the local tmux server's sessions, panes, and free slots served from a live cache
  So that free-slot queries answer sub-second and issue-to-branch joins have a pane to point at

  # The tmux plugin is a read model of the LOCAL tmux server. One long-lived control-mode client
  # (`tmux -C attach`, stdin held open, `refresh-client -f no-output`) streams server-wide
  # structural notifications into a per-plugin SQLite cache; a reconcile poll (`list-panes -a`
  # / `list-sessions`, default 15s) backstops missed events, recomputes free/busy, and marks
  # vanished entities staleSince. Keys are `pane:<session>:<window>.<pane>@<hostId>` (and the
  # session/window/server kinds). It serves via the gqlgen `extend type` glob seam — no webhook,
  # no HTTPRoutes. @local runs against a REAL tmux server on a private socket (`-L sg-test-<pid>`),
  # created per scenario and torn down with a VERIFIED kill (poll until the socket is gone, else
  # SIGKILL the leaked server + attach pids) — hermetic, no credentials, no fake server. @pending only
  # where a second box + the peer plugin are needed. `T` = reconcileIntervalSeconds (test: short).
  #
  # OWNERSHIP: S2 (cross-box free-slot fan-out) is the PEER plugin's; this feature owns only the
  # LOCAL warm-read half, tagged @AC-TMUX-FREESLOTS-WARM (no @S2 here, prd.feature S2 untouched).

  # ---------- Free-slot warm read (local half of S2) ----------

  @tmux @local @AC-TMUX-FREESLOTS-WARM
  Scenario: Free-slot query answers warm in under 1 second over 20 samples
    Given a tmux server on a private socket seeded with idle and busy panes
    And a supergraph server watching that socket with the cache warm
    When `freeSlots` is queried 20 times
    Then every idle pane is returned and no busy pane is offered as free
    And the p95 of the 20 query latencies is under 1s

  # ---------- Event source: control mode is server-wide, passive, and sub-second ----------

  @tmux @local @AC-TMUX-EVENTS-CONTROL
  Scenario: Panes created in a different session are observed sub-second via control mode
    Given a supergraph server watching a private tmux socket via control mode with the reconcile interval set far beyond the observation window
    And a session "s1" the control client is attached to and a second session "s3"
    When 20 panes are split in session "s3", each timed from split to queryable
    Then every new pane became queryable via the control-mode path and the p95 split-to-queryable latency is under 1s
    And the cached pane count equals the real tmux pane count, with no `%output` noise ingested

  @tmux @local @AC-TMUX-NO-HOOK-CLOBBER
  Scenario: The plugin never mutates the operator's global tmux hooks
    Given a private tmux socket with a user-set global hook `after-split-window`
    When a supergraph server runs the tmux plugin against that socket for a full watch-and-reconcile cycle
    Then `show-hooks -g` on the socket is byte-identical before and after the run
    And `grep -R "set-hook" plugins/tmux` returns nothing

  # ---------- Structure + freshness ----------

  @tmux @local @AC-TMUX-PANE-DEATH
  Scenario: A killed pane leaves the free-slot set and is marked stale
    Given a supergraph server watching a private tmux socket with a tracked idle-shell pane
    And the pane appears in `freeSlots`
    When the pane is killed with `kill-pane`
    Then within T the pane is marked staleSince and no longer appears in `freeSlots`

  @tmux @local @AC-TMUX-FREE-BUSY
  Scenario: A pane running a command reads busy and returns free at the prompt
    Given a supergraph server watching a private tmux socket with a tracked idle-shell pane that reads free
    When the pane starts running `sleep 30`
    Then within T the pane is absent from `freeSlots`
    When the command is interrupted and the pane returns to its shell prompt
    Then within T the pane reads free again

  @tmux @local @AC-TMUX-STALE
  Scenario: A pane closed with the control client disabled is healed by the next reconcile
    Given a supergraph server watching a private tmux socket in poll-only mode so only the reconcile heals
    And a tracked pane in a session with a second pane
    When the pane is killed
    Then a query before the next reconcile still shows the pane, by design
    When the reconcile poll runs once
    Then the pane is marked staleSince, bounding worst-case staleness by the reconcile interval

  @tmux @local @AC-TMUX-SERVER-DOWN
  Scenario: Killing the tmux server marks local entities stale and the plugin reconnects
    Given a supergraph server watching a private tmux socket with tracked sessions and panes
    When the tmux server is killed with `kill-server`
    Then within T every local session and pane is marked staleSince
    And the plugin does not exit or panic and `GET /health` still returns HTTP 200
    When a new tmux server is started on the same socket
    Then the next reconcile repopulates the sessions and panes

  @tmux @local @AC-TMUX-POLL-ERROR
  Scenario: A reconcile that errors emits no snapshot and health crosses to stale
    Given a supergraph server whose tmux binary is a stub that succeeds until triggered, watching a private socket in poll-only mode
    And the `tmux` entry in `/health` reads healthy after the first reconcile
    When the stub is switched to exit non-zero on every reconcile
    Then no further `tmux.snapshot` advances health and within the lag threshold the `tmux` entry crosses to stale

  # ---------- Join key + grammar ----------

  @tmux @local @AC-TMUX-PANE-FOR-BRANCH
  Scenario: paneForBranch returns the pane whose worktree is on that branch
    Given a supergraph server watching a private tmux socket
    And a session whose worktree is checked out on branch "plugin/tmux"
    And another session on branch "main"
    When `paneForBranch` is queried for "plugin/tmux"
    Then exactly the pane on "plugin/tmux" is returned and the "main" pane is not

  @tmux @local @AC-TMUX-KEY-GRAMMAR
  Scenario: A live pane key round-trips through the running binary
    Given a supergraph server watching a private tmux socket with a tracked pane
    When `tmuxPanes` is queried
    Then the pane key matches `pane:<session>:<window>.<pane>@<hostId>` and re-parses to those parts
    # Malformed-key rejection (embedded `:`/`.`, unknown kind, empty index, missing @hostId) is
    # exercised through the compiled parser in plugins/tmux/keys_test.go (TestParsePaneKeyRejectsMalformed).

  # ---------- Isolation, cursor, zero-core, budget ----------

  @tmux @local @AC-TMUX-ISOLATION
  Scenario: tmux keeps answering when a sibling plugin panics
    Given a supergraph server watching a private tmux socket alongside a sibling plugin that panics in Start
    When the sibling plugin's panic is induced
    Then the "tmux" entry in `/health` is not stale and `freeSlots` still answers
    And the tmux plugin still serves `GET /health` with HTTP 200

  @tmux @local @AC-TMUX-CURSOR
  Scenario: The reconcile cursor persists across a restart
    Given a supergraph server watching a private tmux socket that has run at least one reconcile
    When the tmux server is killed and the supergraph server is restarted against the same data dir
    Then the `snapshot:lastAt` cursor reported in `/health` is resumed from disk, not reset

  @tmux @local @AC-TMUX-ZEROCORE
  Scenario: Adding the tmux plugin touches zero files under core/
    Given the tmux plugin exists under `plugins/tmux/`, registered via `graph/plugins_import.go` and regenerated `graph/`
    When I check `git diff --stat core/` against main
    Then it reports 0 files changed
    And a supergraph server watching a private tmux socket serves a "tmux" entry in `/health`

  @tmux @local @AC-TMUX-LOC
  Scenario: The tmux plugin stays within its LOC budget
    Given the tmux plugin source under `plugins/tmux/`
    When `make loc-tmux` counts non-comment non-blank lines of the non-test Go files
    Then the count is at most 820

  # ---------- Cross-box (needs the peer plugin + a second box) ----------

  @tmux @pending @AC-TMUX-STALE-PEER
  Scenario: A stopped box shows its tmux data as stale on its peers within 30 seconds
    Given the peer plugin mirrors a second box's tmux data
    When the second box stops
    Then within 30s the peer shows the second box's tmux sessions and panes as stale-since-T
    And no peer-of-peer rows exist
    And evidence is captured: "stopped box tmux data stale-since-T within 30s, no peer-of-peer rows, via a query screenshot and a grep"

  # --- AC Coverage Map ---
  # AC-TMUX-FREESLOTS-WARM: "Free-slot query p95 < 1s warm (local half of S2; S2 fan-out is peer's)" -> Scenario: Free-slot query answers warm in under 1 second over 20 samples
  # AC-TMUX-EVENTS-CONTROL: "Control mode server-wide + sub-second, poll interval beyond window so only the push can satisfy it" -> Scenario: Panes created in a different session are observed sub-second via control mode
  # AC-TMUX-NO-HOOK-CLOBBER:"Plugin never mutates operator global hooks (D2)" -> Scenario: The plugin never mutates the operator's global tmux hooks
  # AC-TMUX-PANE-DEATH:     "Killed pane leaves freeSlots, marked stale" -> Scenario: A killed pane leaves the free-slot set and is marked stale
  # AC-TMUX-FREE-BUSY:      "Busy pane excluded, returns free at prompt (D5)" -> Scenario: A pane running a command reads busy and returns free at the prompt
  # AC-TMUX-STALE:          "Negative control — staleness bounded by reconcile interval" -> Scenario: A pane closed with the control client disabled is healed by the next reconcile
  # AC-TMUX-SERVER-DOWN:    "Server death -> local stale + backoff reconnect" -> Scenario: Killing the tmux server marks local entities stale and the plugin reconnects
  # AC-TMUX-POLL-ERROR:     "Errored/timed-out reconcile emits no snapshot -> health stale (owner T1)" -> Scenario: A reconcile that errors emits no snapshot and health crosses to stale
  # AC-TMUX-PANE-FOR-BRANCH:"issue<->branch join pane" -> Scenario: paneForBranch returns the pane whose worktree is on that branch
  # AC-TMUX-KEY-GRAMMAR:    "Key round-trip via binary; malformed rejection unit-tested" -> Scenario: A live pane key round-trips through the running binary
  # AC-TMUX-ISOLATION:      "F5 tmux half — keeps answering when sibling panics" -> Scenario: tmux keeps answering when a sibling plugin panics
  # AC-TMUX-CURSOR:         "Cursor persists across restart" -> Scenario: The reconcile cursor persists across a restart
  # AC-TMUX-ZEROCORE:       "S5 — zero core edit" -> Scenario: Adding the tmux plugin touches zero files under core/
  # AC-TMUX-LOC:            "LOC budget <= 820" -> Scenario: The tmux plugin stays within its LOC budget
  # AC-TMUX-STALE-PEER:     "Stale peer (cross-box, @pending, was F8; F8 stays peer-owned in prd.feature)" -> Scenario: A stopped box shows its tmux data as stale on its peers within 30 seconds
