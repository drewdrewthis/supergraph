Feature: Claude plugin — session state from lifecycle hooks + transcript tail
  As the orchardist and the owner
  I want every running Claude Code session as a graph row joined to its issue, PR, and tmux pane
  So that "what is touching issue #N" answers with live session state, and a stale session never hides

  # The claude plugin ingests Claude Code session state from two channels (EDR docs/edr/claude.md):
  # (a) lifecycle HOOKS POSTed to /plugins/claude/hook — sub-ms live state, the only source of
  # TMUX_PANE and permission-input state; (b) an fsnotify TAIL of ~/.claude/projects/*/<sid>.jsonl —
  # the correctness floor that backfills pre-install/subagent sessions and enriches with gitBranch,
  # pr-link, and model that the hook payload lacks. A session seen by both is deduped by
  # (sessionId, event, ts). Rows are keyed session:<sessionId>@<hostId>. PROMPT, RESPONSE, and
  # tool_input bodies are never stored — only structural metadata crosses the mesh. @local drives
  # everything by POSTing synthetic hook payloads and writing synthetic transcript JSONL against a
  # temp projectsDir + temp settingsPath — no live Claude session. @live needs a real tmux session
  # and is @pending until run on a box.

  # ---------- PRD ACs (claude scope) ----------

  @claude @local @F1
  Scenario: F1 — a claude event is ingest-to-queryable p95 under 1 second over 20 samples
    Given a supergraph server started with the claude plugin and data dir <tmp>
    When 20 synthetic `PreToolUse` hook payloads for distinct sessions are POSTed to `/plugins/claude/hook`
    Then each session is queryable and the p95 of POST-to-queryable over the 20 samples is under 1s
    And evidence is captured: "p95 ingest-to-queryable < 1s over 20 samples, via paired log timestamps"

  @claude @local @F5
  Scenario: F5 — a panicking claude plugin goes stale while other plugins keep answering
    Given a supergraph server started with the claude plugin, a second "fakeok" plugin that never panics, and data dir <tmp>
    When I induce a panic in the claude plugin's Start via its panic-inject path
    And I wait for the panic to be detected
    Then the "claude" entry in `/health` has state "stale"
    And `GET /health` still returns HTTP 200
    And the "fakeok" entry in `/health` has state "ok"
    And `supergraph query '{ __typename }'` still returns HTTP 200

  @claude @local @S4
  Scenario: S4 — a query for issue #N returns a source-tagged claude row, absent source shows a stale marker
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And a session on git branch `issue42/spike` has been folded from hook events
    When I query `touching` for issue number 42
    Then at least one `ClaudeSession` row is returned, source-tagged "claude", with `issueNumber` 42
    When I query `touching` for issue number 99 which has no session
    Then the claude source is present with an explicit `staleSince` marker, never omitted
    And evidence is captured: "one claude row for issue #N source-tagged, absent source shows staleSince, via a query result screenshot"

  # ---------- Ingest + state machine ----------

  @claude @local @AC-CLAUDE-HOOK-INGEST
  Scenario: A hook lifecycle sequence folds a session through idle, working, and back to idle
    Given a supergraph server started with the claude plugin and data dir <tmp>
    When a `SessionStart` hook payload for session `S1` is POSTed to `/plugins/claude/hook`
    Then `claudeSession(sessionId: "S1")` has state "idle"
    When a `UserPromptSubmit` hook payload for `S1` is POSTed
    Then `claudeSession(sessionId: "S1")` has state "working"
    When a `PreToolUse` hook payload for `S1` with `tool_name` "Bash" is POSTed
    Then `claudeSession(sessionId: "S1")` has state "working" and `lastTool` "Bash" and `toolCalls` 1
    When a `Stop` hook payload for `S1` is POSTed
    Then `claudeSession(sessionId: "S1")` has state "idle"

  @claude @local @AC-CLAUDE-STATE-MACHINE
  Scenario: A permission notification flips to input but the idle nag does not
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And session `S2` is in state "working"
    When a `Notification` hook payload for `S2` with a permission message is POSTed
    Then `claudeSession(sessionId: "S2")` has state "input"
    And the stored row carries no copy of the notification message body
    When a `Notification` hook payload for `S2` with message "Claude is waiting for your input" is POSTed
    Then `claudeSession(sessionId: "S2")` is not in state "input"
    When a `Stop` hook payload for `S2` is POSTed
    Then `claudeSession(sessionId: "S2")` has state "idle"

  @claude @local @AC-CLAUDE-PANE
  Scenario: TMUX_PANE at hook time builds a ClaudeInstance joined to a tmux pane
    Given a supergraph server started with the claude plugin and data dir <tmp>
    When a `PreToolUse` hook payload for session `S3` is POSTed with `TMUX_PANE` "%23" in its environment
    Then a `ClaudeInstance` exists with `pane` "%23" and a non-zero `pid` whose `session` is `S3`
    And a `claude.instance.updated` envelope is emitted with pane "%23"
    And the instance's `pane` "%23" is the join key a `TmuxPane` row keys on

  @claude @local @AC-CLAUDE-ISSUE-JOIN
  Scenario: issueNumber is derived from the git branch and confirmed by a pr-link record
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And a synthetic transcript for session `S4` on git branch `issue7/foo` with a `pr-link` record for PR 12
    When the transcript tail folds session `S4`
    Then `claudeSession(sessionId: "S4")` has `issueNumber` 7 and `prNumber` 12 and `prUrl` ending "/pull/12"

  # ---------- Transcript tail: backfill, enrich, dedup, cursor ----------

  @claude @local @AC-CLAUDE-TAIL-BACKFILL
  Scenario: A session with no hook events is discovered by the transcript tail within one scan
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And a synthetic transcript for session `S5` exists under the projects dir with no hook ever POSTed
    When the transcript tail scans the projects dir once
    Then `claudeSession(sessionId: "S5")` exists, folded from the transcript alone
    And evidence is captured: "a pre-install session appears via the tail within one scan, no hook required, via a log"

  @claude @local @AC-CLAUDE-TAIL-ENRICH
  Scenario: The tail adds fields the hook payload does not carry
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And session `S6` was folded from a hook payload that carried no gitBranch, model, or PR
    When the transcript tail folds `S6` from a transcript carrying gitBranch `plugin/claude`, model `claude-fable-5-1`, and a `pr-link` for PR 8
    Then `claudeSession(sessionId: "S6")` now has `gitBranch` "plugin/claude", `model` "claude-fable-5-1", and `prNumber` 8

  @claude @local @AC-CLAUDE-DEDUP
  Scenario: The same transition seen via hook and tail yields one row and no double-count
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And a `PreToolUse` transition for session `S7` has been folded from a hook payload
    When the transcript tail later observes the same `PreToolUse` transition for `S7`
    Then exactly one `claudeSession(sessionId: "S7")` row exists
    And its `toolCalls` count is 1, not 2

  @claude @local @AC-CLAUDE-CURSOR
  Scenario: The per-transcript byte offset persists across a restart and resumes without re-reading
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And the tail has read session `S8`'s transcript up to byte offset T
    When the server is stopped and restarted on the same data dir <tmp>
    Then the `tail:` cursor for `S8`'s transcript is still T after the restart
    And the next scan resumes from offset T and does not re-fold records before T

  # ---------- Liveness + privacy (negative controls) ----------

  @claude @local @AC-CLAUDE-STALE
  Scenario: A session whose process is dead shows staleSince without a SessionEnd
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And session `S9` is folded with a `pid` for a process that is not running, and no `SessionEnd` arrived
    When the pid-liveness sweep runs once
    Then `claudeSession(sessionId: "S9")` has a non-null `staleSince`
    And the row is still present, marked stale, never deleted
    And evidence is captured: "a crashed session is marked staleSince by the liveness sweep, via a log"

  @claude @local @AC-CLAUDE-PRIVACY
  Scenario: Prompt, response, and tool_input bodies are never persisted
    Given a supergraph server started with the claude plugin and data dir <tmp>
    When session `S10` is folded from a `UserPromptSubmit` with prompt "SECRET-PROMPT-STRING", a `PreToolUse` with `tool_input` containing "SECRET-CMD-STRING", and a transcript assistant text "SECRET-RESPONSE-STRING"
    Then `claudeSession(sessionId: "S10")` has `lastTool` and `toolCalls` set
    And a raw dump of `<tmp>/claude.db` contains none of "SECRET-PROMPT-STRING", "SECRET-CMD-STRING", or "SECRET-RESPONSE-STRING"

  # ---------- Install + contract ----------

  @claude @local @AC-CLAUDE-INSTALL-IDEMPOTENT
  Scenario: The settings.json hook merge is additive and idempotent
    Given a settings file at <settingsPath> already containing an unrelated `PreToolUse` hook
    When `supergraph install` merges the claude hook block into <settingsPath>
    Then each of the 7 lifecycle events wires a `supergraph claude-hook` command
    And the pre-existing unrelated `PreToolUse` hook is still present
    When `supergraph install` runs a second time against <settingsPath>
    Then no duplicate `supergraph claude-hook` entry is added to any event array

  @claude @local @AC-CLAUDE-LOC
  Scenario: Production LOC for the claude plugin stays within the 700-line budget
    Given the claude plugin source under `plugins/claude`
    When `make loc-claude` counts non-comment, non-blank prod lines excluding tests
    Then the count is 700 or fewer
    And evidence is captured: "plugins/claude prod LOC <= 700, via make loc-claude output"

  @claude @local @AC-CLAUDE-ZEROCORE
  Scenario: The claude plugin compiles in without touching core
    Given the claude plugin package and its blank import in graph/plugins_import.go
    When `git diff --stat core/` is run
    Then it reports 0 files changed

  # ---------- Live proof (honest @pending) ----------

  @claude @live @pending @AC-CLAUDE-PANE
  Scenario: A real Claude Code session in a tmux pane maps to a ClaudeInstance end to end
    Given the claude plugin is running with its hook installed on a box running tmux
    When a real Claude Code session runs a tool call inside a tmux pane
    Then a `ClaudeInstance` appears with that session's real `pane` and live `pid`
    And it joins to the tmux plugin's `TmuxPane` row for the same pane
    And evidence is captured: "a live session maps pane->pid->session, via a query screenshot"

  # --- AC Coverage Map ---
  # F1  → Scenario: F1 — a claude event is ingest-to-queryable p95 under 1 second over 20 samples
  # F5  → Scenario: F5 — a panicking claude plugin goes stale while other plugins keep answering
  # S4  → Scenario: S4 — a query for issue #N returns a source-tagged claude row, absent source shows a stale marker
  # AC-CLAUDE-HOOK-INGEST     → hook lifecycle sequence folds idle/working/idle
  # AC-CLAUDE-STATE-MACHINE   → permission notification flips to input, idle nag does not
  # AC-CLAUDE-PANE            → TMUX_PANE builds a ClaudeInstance joined to a tmux pane (+ @live @pending)
  # AC-CLAUDE-ISSUE-JOIN      → issueNumber from branch, confirmed by pr-link
  # AC-CLAUDE-TAIL-BACKFILL   → a hook-less session is discovered by the tail within one scan
  # AC-CLAUDE-TAIL-ENRICH     → the tail adds gitBranch/model/PR the hook lacks
  # AC-CLAUDE-DEDUP           → same transition via hook and tail yields one row
  # AC-CLAUDE-CURSOR          → per-transcript byte offset persists across restart
  # AC-CLAUDE-STALE           → dead pid shows staleSince without a SessionEnd
  # AC-CLAUDE-PRIVACY         → prompt/response/tool_input bodies never persisted
  # AC-CLAUDE-INSTALL-IDEMPOTENT → settings.json hook merge is additive + idempotent
  # AC-CLAUDE-LOC             → prod LOC <= 700
  # AC-CLAUDE-ZEROCORE        → git diff --stat core/ = 0

  # <!-- ACs ready for ac-reviewer -->
