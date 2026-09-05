Feature: Claude plugin — session state from lifecycle hooks + transcript tail
  As the orchardist and the owner
  I want every running Claude Code session as a graph row joined to its issue, PR, and tmux pane
  So that "what is touching issue #N" answers with live session state, and a stale session never hides

  # The claude plugin ingests Claude Code session state from two channels (EDR docs/edr/claude.md):
  # (a) lifecycle HOOKS POSTed to /plugins/claude/hook — sub-ms live state, the only source of
  # TMUX_PANE and permission-input state; (b) a poll-scan TAIL of ~/.claude/projects/*/<sid>.jsonl —
  # the correctness floor that backfills pre-install sessions and enriches with gitBranch,
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

  @claude @local @F5
  Scenario: F5 — a panicking claude plugin goes stale while other plugins keep answering
    Given a supergraph server started with the claude plugin, a second "fakeok" plugin that never panics, and data dir <tmp>
    When I induce a panic in the claude plugin's Start via its panic-inject path
    And I wait for the panic to be detected
    Then the "claude" entry in `/health` has state "stale"
    And `GET /health` still returns HTTP 200
    And the "fakeok" entry in `/health` has state "ok"
    And `supergraph query '{ __typename }'` still returns HTTP 200

  # This scenario proves only the CLAUDE HALF of PRD S4 (a source-tagged claude row for
  # issue #N). The umbrella S4 — one query joining github AND claude and stamping a
  # staleSince marker on an absent source — is a cross-plugin `touching` resolver that
  # stays @pending in prd.feature until the join lands. Retagged from @S4 so it never
  # over-claims the cross-source assertion.
  @claude @local @AC-CLAUDE-S4-HALF
  Scenario: S4 (claude half) — a claudeSessions query filtered by issue number returns the source-tagged row
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And a session on git branch `issue42/spike` has been folded from hook events
    When I query `claudeSessions` filtered by issue number 42
    Then at least one `ClaudeSession` row is returned, source-tagged "claude", with `issueNumber` 42
    When I query `claudeSessions` filtered by issue number 99 which has no session
    Then no claude row is returned for issue 99, so the umbrella `touching` query supplies the absent-source `staleSince` marker (prd.feature @S4, still pending)

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

  # ---------- Hook endpoint: auth + payload validation (negative controls) ----------

  # The hook endpoint is mounted under /plugins/claude/ and sits behind core's own
  # bearer-auth middleware: core wraps EVERY route (including plugin routes) in bearer
  # auth whenever the configured Listen is non-loopback (server.New / core.ListenIsLoopback).
  # So on a token-configured, non-loopback instance a hook POST without a valid Bearer is
  # rejected by core BEFORE the claude handler runs — 401, no envelope, no row. The
  # installed hook command therefore carries the token from config (EDR §Install story).
  @claude @local @AC-CLAUDE-HOOK-AUTH
  Scenario: A hook POST to a token-protected instance is rejected without a valid Bearer
    Given a supergraph server on a non-loopback listen with a bearer token configured and the claude plugin, data dir <tmp>
    When a `SessionStart` hook payload for session `A1` is POSTed to `/plugins/claude/hook` with no Authorization header
    Then the hook response status is 401
    And no `claudeSession(sessionId: "A1")` row exists and no claude envelope was emitted for `A1`
    When a `SessionStart` hook payload for session `A1` is POSTed to `/plugins/claude/hook` with a wrong Bearer token
    Then the hook response status is 401
    And no `claudeSession(sessionId: "A1")` row exists and no claude envelope was emitted for `A1`
    When a `SessionStart` hook payload for session `A1` is POSTed to `/plugins/claude/hook` with the correct Bearer token
    Then the response status is 200 and `claudeSession(sessionId: "A1")` has state "idle"

  @claude @local @AC-CLAUDE-HOOK-SCHEMA
  Scenario: A hook payload that violates the schema is rejected 400 and stores nothing
    Given a supergraph server started with the claude plugin and data dir <tmp>
    When a hook payload with an unknown `hook_event_name` "Bogus" for session `A2` is POSTed to `/plugins/claude/hook`
    Then the response status is 400
    And no `claudeSession(sessionId: "A2")` row exists
    When a hook payload with a non-numeric `issue_number` "not-a-number" for session `A2` is POSTed to `/plugins/claude/hook`
    Then the response status is 400
    And no `claudeSession(sessionId: "A2")` row exists

  # ---------- Transcript tail: backfill, enrich, dedup, cursor ----------

  @claude @local @AC-CLAUDE-TAIL-BACKFILL
  Scenario: A session with no hook events is discovered by the transcript tail within one scan
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And a synthetic transcript for session `S5` exists under the projects dir with no hook ever POSTed
    When the transcript tail scans the projects dir once
    Then `claudeSession(sessionId: "S5")` exists, folded from the transcript alone

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
    When the claude server is stopped and restarted on the same data dir <tmp>
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

  @claude @local @AC-CLAUDE-PRIVACY
  Scenario: Prompt, response, and tool_input bodies are never persisted, emitted, or served
    Given a supergraph server started with the claude plugin and data dir <tmp>
    And an open `claudeSessionUpdated` subscription capturing every claude envelope
    When session `S10` is folded from a `UserPromptSubmit` with prompt "SECRET-PROMPT-STRING", a `PreToolUse` with `tool_input` containing "SECRET-CMD-STRING", and a transcript assistant text "SECRET-RESPONSE-STRING"
    Then `claudeSession(sessionId: "S10")` has `lastTool` and `toolCalls` set
    And a raw dump of `<tmp>/claude.db` contains none of "SECRET-PROMPT-STRING", "SECRET-CMD-STRING", or "SECRET-RESPONSE-STRING"
    And every envelope pushed to the subscription contains none of "SECRET-PROMPT-STRING", "SECRET-CMD-STRING", or "SECRET-RESPONSE-STRING"
    And the `claudeSession(sessionId: "S10")` GraphQL result contains none of "SECRET-PROMPT-STRING", "SECRET-CMD-STRING", or "SECRET-RESPONSE-STRING"

  # ---------- Install + contract ----------

  # Owner decision C1: hook install is OPT-IN. Bare `supergraph install` never writes a
  # settings file — it PRINTS the exact hook JSON block for the user to paste. Only
  # `--install-hook` writes, and only into the `--settings <path>` given (never the real
  # ~/.claude/settings.json), so this @local scenario merges into a temp file. The merge is
  # additive + idempotent, deduped by the command string. NOTE: a freshly-merged hook only
  # starts firing once Claude Code reloads settings — there is a settings-reload latency gap,
  # which is why the transcript tail (not the hook) is the correctness floor (EDR §Decision).
  @claude @local @AC-CLAUDE-INSTALL-IDEMPOTENT
  Scenario: Bare install prints the hook block; --install-hook merges idempotently into a temp settings file
    Given a temp settings file at <settingsPath> already containing an unrelated `PreToolUse` hook
    When `supergraph install` runs with no `--install-hook` flag
    Then it prints a hook JSON block naming a `supergraph claude-hook` command for each of the 7 lifecycle events
    And the temp settings file at <settingsPath> is left unchanged
    When `supergraph install --install-hook --settings <settingsPath>` merges the claude hook block
    Then each of the 7 lifecycle events in <settingsPath> wires a `supergraph claude-hook` command
    And the pre-existing unrelated `PreToolUse` hook is still present
    When `supergraph install --install-hook --settings <settingsPath>` runs a second time
    Then no duplicate `supergraph claude-hook` entry is added to any event array

  @claude @local @AC-CLAUDE-LOC
  Scenario: Production LOC for the claude plugin stays within the 750-line budget
    Given the claude plugin source under `plugins/claude`
    When `make loc-claude` counts non-comment, non-blank prod lines excluding tests
    Then the count is 750 or fewer

  @claude @local @AC-CLAUDE-ZEROCORE
  Scenario: The claude plugin compiles in without touching core
    Given the claude plugin package and its blank import in graph/plugins_import.go
    When `git diff --stat core/` is run after the claude plugin compiles in
    Then it reports 0 core files changed

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
  # AC-CLAUDE-S4-HALF         → S4 (claude half): claudeSessions filtered by issueNumber; umbrella S4 stays @pending in prd.feature
  # AC-CLAUDE-HOOK-INGEST     → hook lifecycle sequence folds idle/working/idle
  # AC-CLAUDE-HOOK-AUTH       → hook POST behind core bearer auth on a non-loopback instance: 401 without/with wrong token, no row/envelope
  # AC-CLAUDE-HOOK-SCHEMA     → unknown hook_event_name / non-numeric issue_number → 400, nothing stored
  # AC-CLAUDE-STATE-MACHINE   → permission notification flips to input, idle nag does not
  # AC-CLAUDE-PANE            → TMUX_PANE builds a ClaudeInstance joined to a tmux pane (+ @live @pending)
  # AC-CLAUDE-ISSUE-JOIN      → issueNumber from branch, confirmed by pr-link
  # AC-CLAUDE-TAIL-BACKFILL   → a hook-less session is discovered by the tail within one scan
  # AC-CLAUDE-TAIL-ENRICH     → the tail adds gitBranch/model/PR the hook lacks
  # AC-CLAUDE-DEDUP           → same transition via hook and tail yields one row
  # AC-CLAUDE-CURSOR          → per-transcript byte offset persists across restart
  # AC-CLAUDE-STALE           → dead pid shows staleSince without a SessionEnd
  # AC-CLAUDE-PRIVACY         → prompt/response/tool_input bodies never persisted, emitted (WS), or served (GraphQL)
  # AC-CLAUDE-INSTALL-IDEMPOTENT → settings.json hook merge is additive + idempotent
  # AC-CLAUDE-LOC             → prod LOC <= 750 (measured 710 + 5% headroom; helpers moved to plugins/internal/pluginconfig)
  # AC-CLAUDE-ZEROCORE        → git diff --stat core/ = 0

  # <!-- ACs ready for ac-reviewer -->
