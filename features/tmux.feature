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
  # created and torn down per scenario — hermetic, no credentials, no fake server. @pending only
  # where a second box + the peer plugin are needed. `T` = reconcileIntervalSeconds (test: short).

  # ---------- PRD AC (tmux scope) ----------

  @tmux @local @S2
  Scenario: S2 — free-slot query answers warm in under 1 second over 20 samples
    Given a supergraph server started with the tmux plugin and data dir <tmp>
    And a tmux server on a private socket seeded with a realistic set of sessions and panes
    And the plugin cache is warm
    When `supergraph query --op freeSlots` is run 20 times
    Then every free pane is returned and no busy pane is offered as free
    And the p95 of the 20 query latencies is under 1s
    And evidence is captured: "p95 free-slot query latency < 1s warm over 20 samples, via a timing log"

  # ---------- Event source: control mode is server-wide and passive ----------

  @tmux @local @AC-TMUX-EVENTS-CONTROL
  Scenario: A pane created in a different session is observed server-wide via control mode
    Given a supergraph server started with the tmux plugin watching a private tmux socket via control mode
    And a session "s1" the control client is attached to and a second session "s3"
    When a new window is opened and a pane is split in session "s3"
    Then the new pane in "s3" is queryable via `tmuxPanes` within T
    And no `%output` pane-content noise was ingested

  @tmux @local @AC-TMUX-NO-HOOK-CLOBBER
  Scenario: The plugin never mutates the operator's global tmux hooks
    Given a private tmux socket with a user-set global hook `after-split-window`
    When a supergraph server runs the tmux plugin against that socket for a full watch-and-reconcile cycle
    Then `show-hooks -g` on the socket is byte-identical before and after the run
    And `grep -R "set-hook" plugins/tmux` returns nothing

  # ---------- Structure + freshness ----------

  @tmux @local @AC-TMUX-PANE-DEATH
  Scenario: A killed pane leaves the free-slot set and is marked stale
    Given a supergraph server started with the tmux plugin and a tracked idle-shell pane
    And the pane appears in `freeSlots`
    When the pane is killed with `kill-pane`
    Then within T the pane is marked staleSince and no longer appears in `freeSlots`
    And a `tmux.pane.closed` envelope is emitted for its key

  @tmux @local @AC-TMUX-FREE-BUSY
  Scenario: A pane running a command reads busy and returns free at the prompt
    Given a supergraph server started with the tmux plugin and a tracked idle-shell pane that reads free
    When the pane starts running `sleep 30`
    Then within T the pane is absent from `freeSlots`
    When the command finishes and the pane returns to its shell prompt
    Then within T the pane reads free again

  @tmux @local @AC-TMUX-STALE
  Scenario: A pane closed during a control-client reconnect is healed by the next reconcile
    Given a supergraph server started with the tmux plugin and a tracked pane
    And the control-mode client is mid-reconnect so no `%`-notification is observed
    When the pane is killed with no notification reaching the plugin
    Then a query before the next reconcile still shows the pane, by design
    When the reconcile poll runs once
    Then the pane is marked staleSince, bounding worst-case staleness by the reconcile interval
    And evidence is captured: "worst-case staleness is bounded by reconcileIntervalSeconds, via a log"

  @tmux @local @AC-TMUX-SERVER-DOWN
  Scenario: Killing the tmux server marks local entities stale and the plugin reconnects
    Given a supergraph server started with the tmux plugin watching a private tmux socket
    And sessions and panes are tracked
    When the tmux server is killed with `kill-server`
    Then a `tmux.server.down` envelope is emitted
    And within T every local session and pane is marked staleSince
    And the plugin does not exit or panic and backs off to reconnect
    When a new tmux server is started on the same socket
    Then the next reconcile repopulates the sessions and panes

  # ---------- Join key + grammar ----------

  @tmux @local @AC-TMUX-PANE-FOR-BRANCH
  Scenario: paneForBranch returns the pane whose worktree is on that branch
    Given a supergraph server started with the tmux plugin and data dir <tmp>
    And a session whose worktree is checked out on branch "plugin/tmux"
    And another session on branch "main"
    When `paneForBranch` is queried for "plugin/tmux"
    Then exactly the pane on "plugin/tmux" is returned and the "main" pane is not

  @tmux @local @AC-TMUX-KEY-GRAMMAR
  Scenario: A pane key round-trips and a malformed key is rejected
    Given the tmux key grammar `pane:<session>:<window>.<pane>@<hostId>`
    When a pane key is formatted from session, window index, pane index, and hostId
    Then parsing it back yields the same session, window, pane, and hostId
    When a malformed key missing its `@<hostId>` is parsed
    Then the parse returns an error rather than a partial key

  # ---------- Isolation, cursor, zero-core, budget ----------

  @tmux @local @AC-TMUX-ISOLATION
  Scenario: tmux keeps answering when a sibling plugin panics
    Given a supergraph server started with the tmux plugin and a second plugin that panics in Start
    When the sibling plugin's panic is induced
    Then the "tmux" entry in `/health` is not stale and `slots`/`freeSlots` still answer
    And `GET /health` still returns HTTP 200

  @tmux @local @AC-TMUX-CURSOR
  Scenario: The reconcile cursor persists across a restart
    Given a supergraph server started with the tmux plugin that has run at least one reconcile
    When I run `supergraph stop` and then start the server again against the same data dir
    Then the `snapshot:lastAt` cursor reported in `/health` is unchanged, not reset

  @tmux @local @AC-TMUX-ZEROCORE
  Scenario: Adding the tmux plugin touches zero files under core/
    Given the tmux plugin exists under `plugins/tmux/`, registered via `graph/plugins_import.go` and regenerated `graph/`
    When I check `git diff --stat core/`
    Then it reports 0 files changed
    And a supergraph server started with the tmux plugin serves a "tmux" entry in `/health`

  @tmux @local @AC-TMUX-LOC
  Scenario: The tmux plugin stays within its LOC budget
    Given the tmux plugin source under `plugins/tmux/`
    When `make loc-tmux` counts non-comment non-blank lines of the non-test Go files
    Then the count is at most 600

  # ---------- Cross-box (needs the peer plugin + a second box) ----------

  @tmux @pending @F8
  Scenario: F8 — a stopped box shows its tmux data as stale on its peers within 30 seconds
    Given the peer plugin mirrors a second box's tmux data
    When the second box stops
    Then within 30s the peer shows the second box's tmux sessions and panes as stale-since-T
    And no peer-of-peer rows exist
    And evidence is captured: "stopped box tmux data stale-since-T within 30s, no peer-of-peer rows, via a query screenshot and a grep"

  # --- AC Coverage Map ---
  # S2:                     "Free-slot query p95 < 1s warm" -> Scenario: S2 — free-slot query answers warm in under 1 second over 20 samples
  # AC-TMUX-EVENTS-CONTROL: "Control mode is server-wide + passive" -> Scenario: A pane created in a different session is observed server-wide via control mode
  # AC-TMUX-NO-HOOK-CLOBBER:"Plugin never mutates operator global hooks (D2)" -> Scenario: The plugin never mutates the operator's global tmux hooks
  # AC-TMUX-PANE-DEATH:     "Killed pane leaves freeSlots, marked stale" -> Scenario: A killed pane leaves the free-slot set and is marked stale
  # AC-TMUX-FREE-BUSY:      "Busy pane excluded, returns free at prompt (D5)" -> Scenario: A pane running a command reads busy and returns free at the prompt
  # AC-TMUX-STALE:          "Negative control — staleness bounded by reconcile interval" -> Scenario: A pane closed during a control-client reconnect is healed by the next reconcile
  # AC-TMUX-SERVER-DOWN:    "Server death -> local stale + backoff reconnect" -> Scenario: Killing the tmux server marks local entities stale and the plugin reconnects
  # AC-TMUX-PANE-FOR-BRANCH:"issue<->branch join pane" -> Scenario: paneForBranch returns the pane whose worktree is on that branch
  # AC-TMUX-KEY-GRAMMAR:    "Key round-trip + reject malformed" -> Scenario: A pane key round-trips and a malformed key is rejected
  # AC-TMUX-ISOLATION:      "F5 tmux half — keeps answering when sibling panics" -> Scenario: tmux keeps answering when a sibling plugin panics
  # AC-TMUX-CURSOR:         "Cursor persists across restart" -> Scenario: The reconcile cursor persists across a restart
  # AC-TMUX-ZEROCORE:       "S5 — zero core edit" -> Scenario: Adding the tmux plugin touches zero files under core/
  # AC-TMUX-LOC:            "LOC budget <= 600" -> Scenario: The tmux plugin stays within its LOC budget
  # F8:                     "Stale peer (cross-box, @pending)" -> Scenario: F8 — a stopped box shows its tmux data as stale on its peers within 30 seconds
