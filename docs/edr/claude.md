# EDR: Claude plugin — session state from lifecycle hooks + transcript tail

Engineering Design Record. Owns internals the [PRD](../PRD.md) §5–§6 and [plugin
contract](../plugin-contract.md) leave to the plugin. Act-doc: decisions + steps, not prose.

The claude plugin turns a fleet of running **Claude Code sessions** into graph rows: `ClaudeSession`
(one per `sessionId`) and `ClaudeInstance` (a session pinned to a tmux `pane` + `pid`). It is the
claude side of the cross-plugin join in PRD **S4** (`issue #N` → github rows **and** claude rows), the
freshness surface in **F1**, and a panic-isolation subject in **F5**.

**All design below is measured on this box (Claude Code `2.1.261`), not assumed** — see
[Measured facts](#measured-facts). Prior art is real and running: `orchardist/claude-session-state`'s
`fold-state.sh` reducer already folds every lifecycle hook into a per-session JSON file. We **borrow its
state machine** (verbatim logic, ported to Go) and **diverge on persistence + privacy** (below).

---

## Measured facts (not assumed)

Read off the live box, not from docs:

| Fact | Evidence (measured 2026-09-05) |
|---|---|
| **Hook payload fields** | Existing hooks parse `.session_id`, `.hook_event_name`, `.cwd`, `.transcript_path`, `.tool_name`, `.tool_input`, `.message`, `.prompt`, `.stop_hook_active`, `.source` from stdin JSON (`~/.claude/hooks/*.sh`, `fold-state.sh:14-44`). |
| **Hook events fired** | `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Notification`, `Stop`, `SessionEnd` (`fold-state.sh:65-105`; `~/.claude/settings.json` wires all of them). |
| **`TMUX_PANE` present at hook time** | `fold-state.sh:61` reads `${TMUX_PANE:-}`; live state file has `"pane":"%23"`. This env var is the **only** session→pane source — it is **not** in the transcript. |
| **Session pid** | `$PPID` climbed to the long-lived `*claude*`/`*node*` ancestor (`fold-state.sh:28-39`) — a hook shell's own `$PPID` is transient. |
| **Transcript path** | `~/.claude/projects/<cwd-slug>/<sessionId>.jsonl`, slug = cwd with `/`→`-` (measured: the plugin-github worktree slug). Also handed to hooks as `.transcript_path`. |
| **Transcript record fields** | Every record carries `cwd`, `gitBranch`, `sessionId`, `timestamp`, `version`, `sessionKind` (`bg`/…), `entrypoint`, `isSidechain`, `userType`, `uuid`, `parentUuid`. |
| **Join keys in transcript** | `pr-link` records carry `prNumber`/`prUrl`/`prRepository`; `gitBranch` encodes issue (`issue1/spike-core`, `plugin/github`); assistant records carry `message.model` (`claude-fable-5-1`) and `tool_use` blocks (`name`,`id`,`input`,`caller`); `agent-name` records name subagents. |
| **Live state schema (prior art)** | `~/.local/state/claude-sessions/state/<sid>.json`: `sid,cwd,transcript_path,pid,pane,started_at,last_event,ts,tool_calls,state,message,last_prompt,first_prompt,last_tool,last_response`. |
| **State machine (prior art)** | `SessionStart`→`idle`; `UserPromptSubmit`/tool→`working`; `Notification` (permission)→`input`, but the idle-nag `"waiting for your input"` is **not** `input`; `Stop`→`idle`; `SessionEnd`→row removed (`fold-state.sh:78-105`). |

---

## Decision: event source = **both** (hooks primary, transcript tail secondary)

Same shape as the github plugin's "webhooks primary, reconcile backstop": a **push** channel for
sub-second live state, a **pull** channel for correctness (backfill + enrichment + heal).

| Option | Latency | Coverage | TMUX_PANE | Input/permission state | Enrichment (branch, PR, model) | Install cost |
|---|---|---|---|---|---|---|
| **(a) hooks → `/plugins/claude/hook`** | sub-ms (sync POST) | only sessions started **after** the hook is installed | **yes** (env at hook time) | **yes** (`Notification`) | no (hook payload lacks them) | must register hook in `settings.json` |
| **(b) fsnotify transcript tail** | file-flush lag (~100ms–1s) | **all** sessions incl. pre-install + subagents (`isSidechain`) | **no** | **no** (not in transcript) | **yes** (`gitBranch`,`pr-link`,`model`,`agent-name`) | none |
| **(c) both** ✅ | sub-ms live, tail heals | all | yes (hook) | yes (hook) | yes (tail) | hook optional — tail is the correctness floor |

**Recommendation: (c).** Neither channel alone is sufficient and their gaps are complementary:
`TMUX_PANE` and permission-`input` state exist **only** in the hook payload (measured), so **(b) alone
cannot build `ClaudeInstance` or the `input` state**; enrichment fields (`gitBranch`, `prNumber`,
`model`) and pre-install/subagent sessions exist **only** in the transcript, so **(a) alone misses the
S4 issue-join key and every session older than the install**. Mirroring the github plugin: **the tail
is primary correctness** (heals everything within one scan even if the hook is never installed — the
webhook-vs-reconcile analogy), **the hook is latency + pane + input-state.** A session seen by both is
deduped by `sessionId`+event (below), exactly like github's delivery dedup.

Rejected: **consuming `fold-state.sh`'s state files** as the ingest (fsnotify on
`~/.local/state/claude-sessions/state/`). Tempting — the reducer already exists — but it couples us to
another plugin's private output path, and that reducer **stores prompt/response bodies** we must not
replicate over the mesh ([Privacy](#privacy)). We **port its state-machine logic** into `reducer.go`
instead and own our persistence.

---

## Key grammar

```
session   := "session:" <sessionId> "@" <hostId>
instance  := "instance:" <pane> "@" <hostId>        # pane e.g. %23; a session pinned to a tmux pane+pid
```
- `sessionId` is the Claude Code UUID (`session_id` / transcript filename). `hostId` is the box.
- Event→key: every hook payload and every transcript record folds to its `session:<sid>@<host>` key;
  the `pane` (hook env) adds the `instance:<pane>@<host>` key for the same session.
- **Idempotency / dedup:** `session:<sid>@<host>` is the entity key; a `(sid, hook_event_name, ts)`
  triple dedups a hook POST against the same transition seen later in the transcript tail — the
  envelope `Key` carries `session:<sid>@<host>` (contract §Envelope).

## Envelopes emitted (`{TS,Source:"claude",Type,V:1,Key,Payload}`)

`/health` `lastEventAt` and F1 timing derive from these; tmux/github join on `Key` + `issueNumber`.

- **`claude.session.updated`** — `Payload={sid,state,cwd,gitBranch,issueNumber,model,lastTool,toolCalls}`
  — any state transition (start, prompt, tool, stop) or a tail enrichment upsert.
- **`claude.session.input`** — `Payload={sid,awaitingInput:true}` — a `Notification` permission ask;
  the message **body is not carried** ([Privacy](#privacy)).
- **`claude.session.ended`** — `Payload={sid}` — `SessionEnd`, or the tail/liveness sweep finding the
  session's `pid` dead. The row is marked `staleSince`, not deleted (S4: absent source shows a stale
  marker, never omission).
- **`claude.instance.updated`** — `Payload={sid,pane,pid}` — a hook carried a `TMUX_PANE`; feeds the
  `ClaudeInstance`↔`TmuxPane` join (tmux plugin keys on `pane`).
- **`claude.tool.used`** — `Payload={sid,tool}` — a `PreToolUse`; **tool `name` only, never
  `tool_input`** ([Privacy](#privacy)).

## Freshness + liveness (honest)

- **Live state is push** — a hook POST folds a transition in sub-ms, so F1 (< 1 s ingest-to-queryable)
  is met by construction on the hook path.
- **`staleSince` is derived two ways:** (1) `lastEventAt` crossing the core lag threshold (contract
  §Liveness — a silent session is a stale session), **and** (2) a cheap **pid-liveness** check
  (`ps -p <pid>` / `kill -0`) on the tail sweep — a session whose process is gone is stale even if it
  never emitted `SessionEnd` (crash, `kill`). Analogous to github's negative-control **AC-GH-STALE**.
- **Backfill has no baseline job:** on boot the tail scans `~/.claude/projects/*/` once, folding each
  transcript's current tail into a row — sessions started before the plugin (or before its hook)
  appear within one scan, bounded by per-file read latency, not a bulk job.

## Cross-plugin joins (S4, F8)

- **issue ↔ session:** `issueNumber` is derived from `gitBranch` (`issueN/…` → `N`) and confirmed by
  `pr-link.prNumber` when the transcript carries one. This is PRD §6's "first cross-plugin join key:
  issue number ↔ branch/worktree." S4's claude row is `ClaudeSession` filtered by `issueNumber`.
- **session ↔ pane:** `ClaudeInstance.pane` (from `TMUX_PANE`) joins to the tmux plugin's `TmuxPane`;
  `pane`+`pid` is the instance identity per the PRD sketch.
- **Absent source rule (S4):** a session with no github issue, or an issue with no session, returns a
  row tagged `staleSince`/`null`, never an omitted source.

## Privacy — store metadata, never content

**This is the load-bearing divergence from the prior-art reducer.** `fold-state.sh` stores
`last_prompt`, `first_prompt`, `last_response`, and the `Notification` `message` body — the live state
file above literally holds a full cross-session message and a 500-char first prompt. The supergraph is
**multi-box and peer-mirrored over the mesh** (PRD §6 `peer` plugin), so persisting prompt/response
bodies would **replicate potentially sensitive session content across every box**. The PRD
`ClaudeSession` schema deliberately carries **no text field** — we align to it.

| Field | Stored? | Why |
|---|---|---|
| `sessionId`, `hostId`, `cwd`, `gitBranch`, `issueNumber` | **stored** | structural; join keys; `cwd` is a path, not content |
| `model`, `state`, `lastTool` (name), `toolCalls` (count), `pane`, `pid` | **stored** | operational metadata; `lastTool` is the tool **name** only |
| `startedAt`, `lastEventAt`, `staleSince`, `prNumber`, `prUrl` | **stored** | timing; PR fields are public |
| **prompt text** (`last_prompt`/`first_prompt`) | **redacted** | user content; never crosses the mesh |
| **assistant response text** (`last_response`) | **redacted** | model output content |
| **`tool_input`** (command bodies, file contents, paths) | **redacted** | may contain secrets/paths; only the tool **name** is kept |
| **`Notification` message body** | **redacted** | may name a gated command; stored as `awaitingInput:true` + `state:input` only |

`redact.go` is a **whitelist**: the fold path constructs the stored row from an explicit field list, so
a new transcript field is dropped by default, not leaked. **AC-CLAUDE-PRIVACY** is a negative control:
dump the db after a session with prompts/tool calls and assert **no prompt/response/tool_input bytes
are present**.

## Install story — hook registration is optional, idempotent

Because the **tail is the correctness floor**, the hook is a latency/pane/​input enhancement, never a
correctness dependency — a box that never installs the hook still gets every session via the tail
(minus live-`input` state and `pane`). Registration:

- **`supergraph install` extends to merge the claude hook block into `~/.claude/settings.json`**
  (a `cmd/` delta, counted separately like github's CLI, **not** in the plugin LOC budget). The merge is
  **additive + idempotent**: it appends one `command` entry (`supergraph claude-hook`, a thin stdin→POST
  forwarder) per event array, **deduped by command string** — running it twice adds nothing (the
  existing `settings.json` already holds many hook arrays; we never clobber them). **AC-CLAUDE-INSTALL-IDEMPOTENT**.
- **Documented one-liner fallback** (for users who hand-edit): a `settings.json` snippet in the plugin
  README wiring the 7 events to `curl -s -XPOST 127.0.0.1:7788/plugins/claude/hook -d @-`.
- **Uninstall** removes only entries whose command matches ours.

## SQLite state (add-only, `IF NOT EXISTS`)

- `claude_sessions(sid TEXT PK, host_id, cwd, git_branch, issue_number INT, model, state, last_tool, tool_calls INT, pane, pid INT, pr_number INT, pr_url, started_at, last_event_at, stale_since)` — `ClaudeInstance` is the projection of rows with a non-null `pane` (no second table).
- cursors live in core's `cursors` table: `tail:<transcriptPath>` = last byte offset (resume without re-reading); **AC-CLAUDE-CURSOR**.
- **No prompt/response/tool_input columns exist** — privacy enforced by schema, not just by code.

## Schema (`plugins/claude/schema/claude.graphqls`, extend seam)

```graphql
extend type Query {
  claudeSessions(hostId: String): [ClaudeSession!]!
  claudeSession(sessionId: String!): ClaudeSession
}
extend type Subscription { claudeSessionUpdated(hostId: String): ClaudeSession! }

type ClaudeSession { hostId: String!, sessionId: String!, cwd: String!, gitBranch: String,
  issueNumber: Int, model: String, state: String!, lastTool: String, toolCalls: Int!,
  prNumber: Int, prUrl: String, lastEventAt: Time!, staleSince: Time }
type ClaudeInstance { hostId: String!, pane: String!, pid: Int!, session: ClaudeSession!, staleSince: Time }
```
Types keep the `Claude*` prefix (no upstream to match, PRD §6). `touching(issueNumber)` (core) fans
into `claudeSessions` filtered by `issueNumber` for S4.

## Config keys `[plugins.claude]`

`hostId` (else hostname), `projectsDir` (default `~/.claude/projects`), `stateThresholdSeconds`
(stale lag, default 300), `pidLiveness` (bool, default true), `installHook` (bool — whether
`supergraph install` writes the settings.json block, default true), `settingsPath`
(default `~/.claude/settings.json`, test override), `projectsDir` override drives `@local` tests.

## LOC budget (prod, cap **700**; tests excluded)

Table shows target **Actual** LOC (EDR strip formula: non-comment, non-blank). CLI hook-install delta
lives in `cmd/supergraph/` (~50) and is counted separately, like github's.

| File (`plugins/claude/`) | Target | Responsibility |
|---|---:|---|
| `claude.go` | 110 | wiring: `Register`/`New`/`Name`/`Migrate`/`Start`/`HTTPRoutes`/`CursorReporter`/config |
| `keys.go` | 70 | key grammar (`session:`/`instance:`) + event→key + `issueNumber` from branch |
| `store.go` | 150 | SQLite `claude_sessions`: upsert, get, list, `issueNumber` filter, stale-scan, pid-liveness |
| `hook.go` | 110 | `HTTPRoutes` `/hook`: parse hook payload → `reducer` → emit; pane/input path |
| `reducer.go` | 100 | ported state machine (Start/Prompt/Pre/Post/Notification/Stop/End) shared by hook + tail |
| `tail.go` | 120 | fsnotify `projectsDir` watcher + JSONL parse + byte-offset cursor + backfill + enrich |
| `redact.go` | 30 | whitelist projection: build the stored row, drop prompt/response/tool_input bodies |
| **Total** | **690** | cap **700**; CLI delta (~50) counted separately |

- **AC-CLAUDE-LOC** guards it: a CI step counts non-comment, non-blank prod lines under
  `plugins/claude` excluding `*_test.go` and fails > 700 (same `sed`/`wc` formula as `make loc-github`).

## Failure modes

- Hook never installed → tail covers every session (minus live `input`/`pane`); no correctness loss.
- Hook fires but server down → hook forwarder is fire-and-forget (must **exit 0**: a nonzero PreToolUse
  hook denies the tool call — measured constraint, `fold-state.sh:5`); the tail heals the missed row.
- Transcript mid-write / partial JSONL line → per-line `fromjson?` tolerance (prior art `fold-state.sh:47`).
- Session crash (no `SessionEnd`) → pid-liveness sweep marks `staleSince`.
- Same event via hook **and** tail → deduped by `(sid, event, ts)`.
- Plugin panic in `Start` → core `recover()` marks claude `stale`; tmux/github keep answering (F5).

---

## Acceptance criteria

PRD ACs (claude scope) + plugin ACs are the BDD contract in `features/claude.feature`. `@local` runs by
POSTing synthetic hook payloads and writing synthetic transcript JSONL against a temp `projectsDir` +
temp `settingsPath` — no live Claude session needed. One honest `@live @pending` covers a real
tmux-session round-trip.

**PRD ids claimed (claude scope):**
- **F1** — a claude event is ingest-to-queryable p95 < 1 s (hook POST → query), over 20 samples.
- **F5** — the claude plugin panicking shows `stale` while `_health` + tmux/github keep answering.
- **S4** — a query for issue #N returns ≥ 1 `ClaudeSession` row, source-tagged; an absent source shows
  a `staleSince` marker, never omission.

**Plugin ACs `@AC-CLAUDE-*`:**
`HOOK-INGEST` (a SessionStart→UserPromptSubmit→PreToolUse→Stop hook sequence folds `idle→working→idle`,
queryable) · `STATE-MACHINE` (`Notification` permission→`input`; the `"waiting for your input"` nag
stays `idle`; `Stop`→`idle`) · `PANE` (`TMUX_PANE` at hook time builds a `ClaudeInstance{pane,pid}`
joining to a `TmuxPane`) · `ISSUE-JOIN` (`issueNumber` from `gitBranch` + confirmed by `pr-link`, drives
S4) · `TAIL-BACKFILL` (a session with **no** hook events is discovered by the transcript tail within one
scan) · `TAIL-ENRICH` (the tail adds `gitBranch`/`model`/`prNumber`/`prUrl` the hook lacks) · `STALE`
(negative control: a session whose `pid` is dead shows `staleSince` with no `SessionEnd`) · `PRIVACY`
(negative control: after a session with prompts + tool calls, the db holds **no** prompt/response/
tool_input bytes) · `INSTALL-IDEMPOTENT` (the settings.json hook merge runs twice → no duplicate hook
entries, existing hooks untouched) · `DEDUP` (the same transition via hook **and** tail → one row, no
double-count) · `CURSOR` (the per-transcript byte offset persists across restart and resumes) ·
`ZEROCORE` (`git diff --stat core/` = 0) · `LOC` (prod LOC ≤ 700).

## Coder step plan — two file-partitioned waves (no shared-file contention)

**Wave 1 — plugin** (each file one coder task):
1. `plugins/claude/keys.go` + `store.go` + `Migrate` (one table) — key grammar + session store + pid-liveness.
2. `plugins/claude/reducer.go` — port `fold-state.sh`'s state machine to Go (pure fn: prev+event→next).
3. `plugins/claude/redact.go` — whitelist projection (the privacy guard).
4. `plugins/claude/hook.go` — `HTTPRoutes` `/hook`: payload → reducer → emit; pane/input path.
5. `plugins/claude/tail.go` — fsnotify watcher + JSONL parse + offset cursor + backfill + enrich.
6. `plugins/claude/claude.go` — wiring (`Register`/`Start`/`HTTPRoutes`/`CursorReporter`); blank import
   in `graph/plugins_import.go`; `plugins/claude/schema/claude.graphqls`.
7. `cmd/supergraph/` — `claude-hook` stdin→POST forwarder + idempotent `settings.json` merge in `install`.
8. CI `loc-claude` (AC-CLAUDE-LOC) + `git diff --stat core/`=0 (AC-CLAUDE-ZEROCORE).

**Wave 2 — godog steps** (wires `features/claude.feature`; own file):
9. `features/steps_claude_test.go` — synthetic hook POST + synthetic JSONL writer + temp settings.json;
   p95 harness for F1 (N≥20); privacy/dedup/cursor/pane assertions.

Wave 1 tasks 1–5 are file-disjoint after none (all independent); task 6 wires them; task 2 (reducer) is
the judgment-light port, task 5 (tail) is the judgment-heavy core.

## Handoff
- ACs ready for ac-reviewer (see `features/claude.feature`; `<!-- ACs ready for ac-reviewer -->`).
- Implementation → coder per the two-wave plan; `tail.go` is the judgment-heavy core, `reducer.go` a
  measured port of `fold-state.sh`.
