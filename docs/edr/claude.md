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

> **Implementation note (tail mechanism):** the transcript tail is a **periodic poll scan** of
> `projectsDir` (default every `scanIntervalSeconds`), **not** an fsnotify watcher. fsnotify would add a
> new module dependency for no AC-visible gain: every claude AC drives the tail by an explicit "scan
> once" step, and the backstop only needs to heal within one interval, not sub-file-flush. A poll scan
> keeps the plugin dependency-free and the byte-offset cursor (below) makes each scan resume-cheap. The
> two-channel design (push hook + pull tail) is unchanged; only the pull trigger is a ticker, not inotify.

## Decision: event source = **both** (hooks primary, transcript tail secondary)

Same shape as the github plugin's "webhooks primary, reconcile backstop": a **push** channel for
sub-second live state, a **pull** channel for correctness (backfill + enrichment + heal).

| Option | Latency | Coverage | TMUX_PANE | Input/permission state | Enrichment (branch, PR, model) | Install cost |
|---|---|---|---|---|---|---|
| **(a) hooks → `/plugins/claude/hook`** | sub-ms (sync POST) | only sessions started **after** the hook is installed | **yes** (env at hook time) | **yes** (`Notification`) | no (hook payload lacks them) | must register hook in `settings.json` |
| **(b) poll-scan transcript tail** | scan-interval lag (default 5s) | **all** sessions incl. pre-install + subagents (`isSidechain`) | **no** | **no** (not in transcript) | **yes** (`gitBranch`,`pr-link`,`model`,`agent-name`) | none |
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
| `sessionId`, `hostId`, `cwd`, `gitBranch`, `issueNumber` | **stored** | structural; join keys; `cwd` is a path, not content — **but a path can embed the OS username** (`/home/<user>/…`, `/Users/<user>/…`), which is stored and emitted, so it is not fully anonymous |
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

## Install story — hook registration is OPT-IN, idempotent (owner decision C1)

Because the **tail is the correctness floor**, the hook is a latency/pane/​input enhancement, never a
correctness dependency — a box that never installs the hook still gets every session via the tail
(minus live-`input` state and `pane`). So hook install is **opt-in, default off**:

- **Bare `supergraph install-claude-hook` never writes a settings file.** It **PRINTS the exact hook JSON block**
  (the 7 lifecycle events, each wiring a `supergraph claude-hook` command) to stdout for the user to
  paste into their own `~/.claude/settings.json`. This is the safe default: we do not touch a user's
  settings unless asked.
- **`supergraph install-claude-hook --install-hook --settings <path>`** opts in to writing: it merges the block
  into the `--settings <path>` file (default `~/.claude/settings.json`; tests pass a temp path so the
  real one is never touched). The merge is **additive + idempotent**: it appends one `command` entry
  (`supergraph claude-hook`, a thin stdin→POST forwarder) per event array, **deduped by command
  string** — running it twice adds nothing, and pre-existing unrelated hooks are never clobbered.
  **AC-CLAUDE-INSTALL-IDEMPOTENT**.
- **Hook auth (AC-CLAUDE-HOOK-AUTH):** the `/plugins/claude/hook` route sits behind **core's own
  bearer-auth middleware**, which wraps every route when `listen` is non-loopback
  (`server.New`/`core.ListenIsLoopback`). On such an instance a hook POST without/with a wrong Bearer
  is rejected **by core, before the claude handler runs** — 401, no envelope, no row. The printed/merged
  hook command therefore **includes the box's configured token** (`-H "Authorization: Bearer <token>"`)
  so the local forwarder authenticates. On a loopback-only box no token is needed (core leaves loopback
  open), matching the zero-config local case.
- **Settings-reload latency gap:** a freshly-merged hook starts firing only after Claude Code reloads
  its settings — another reason the tail, not the hook, is the correctness floor.
- **Uninstall** removes only entries whose command matches ours.

## SQLite state (add-only, `IF NOT EXISTS`)

- `claude_sessions(sid TEXT PK, host_id, cwd, git_branch, issue_number INT, model, state, last_tool, tool_calls INT, pane, pid INT, pr_number INT, pr_url, started_at, last_event_at, stale_since)` — `ClaudeInstance` is the projection of rows with a non-null `pane` (no second table).
- cursors live in core's `cursors` table: `tail:<transcriptPath>` = last byte offset (resume without re-reading); **AC-CLAUDE-CURSOR**.
- **No prompt/response/tool_input columns exist** — privacy enforced by schema, not just by code.

## Schema (`plugins/claude/schema/claude.graphqls`, extend seam)

```graphql
extend type Query {
  claudeSessions(hostId: String, issueNumber: Int): [ClaudeSession!]!
  claudeSession(sessionId: String!): ClaudeSession
  claudeInstances(hostId: String): [ClaudeInstance!]!
}
extend type Subscription { claudeSessionUpdated(hostId: String): ClaudeEvent! }

type ClaudeSession { hostId: String!, sessionId: String!, cwd: String!, gitBranch: String,
  issueNumber: Int, model: String, state: String!, lastTool: String, toolCalls: Int!,
  prNumber: Int, prUrl: String, startedAt: Time!, lastEventAt: Time!, staleSince: Time }
type ClaudeInstance { hostId: String!, pane: String!, pid: Int!, session: ClaudeSession!, staleSince: Time }
# ClaudeEvent mirrors core.Envelope (Payload flattened to a string), matching TemplateEvent/GithubEvent
# so the subscription resolver stays a plain envelope→model mapping — and so AC-CLAUDE-PRIVACY can read
# the emitted payload straight off the socket and assert it carries no prompt/response/tool_input bytes.
type ClaudeEvent { ts: Time!, type: String!, v: Int!, key: String!, payload: String! }
```
Types keep the `Claude*` prefix (no upstream to match, PRD §6). The `issueNumber` filter on
`claudeSessions` is the claude half of S4 (**AC-CLAUDE-S4-HALF**); the umbrella `touching(issueNumber)`
join (core, over github+claude, with the absent-source `staleSince` marker) stays PRD-`@pending`.
**Deviations from the first sketch** (kept minimal, both to serve the sharpened ACs and reuse the proven
template/github patterns): the subscription returns a raw `ClaudeEvent` envelope (not `ClaudeSession`)
so the privacy AC can inspect the emitted payload; `claudeInstances` and the `issueNumber` filter are
added for the PANE and S4-half ACs; `startedAt` is surfaced. The query resolvers reach the running
plugin through a package-level `live atomic.Pointer[Plugin]` singleton (`claude.go`) that `Migrate`
publishes and the resolvers `Load` — the same zero-core-edit seam: it lets `graph/` read session state
through the plugin's package funcs **without** a new core-injected `Resolver` field, so no file under
`core/` changes. This is the one narration of the seam; the resolver/schema comments only point back here.

## Client CLI (`supergraph query --op --plugin claude`)

The `query` command takes `--plugin <name>` (default `github`). Named ops load from
`./plugins/<plugin>/queries/NAME.graphql`. github posts `{op,variables}` to its own
`/plugins/github/graphql` op route (which keys each returned node); claude exposes **no**
op route, so `--plugin claude` **falls back to posting the loaded query TEXT** as
`{query,variables}` to core `/graphql` — a plain read, no plugin LOC and no keying, which
is all a session read needs. Shipped ops: `sessionsForIssue.graphql` (`$issueNumber`, the
S4 claude half) and `instances.graphql` (`$hostId`, the tmux-pinned instances).

## Config keys `[plugins.claude]`

`hostId` (else hostname), `projectsDir` (default `~/.claude/projects`), `pidLiveness` (bool, default
true), `scanIntervalSeconds` (tail poll cadence, default 5), `retentionDays` (prune sessions/folds whose
last event is older than this on each tail tick, default 30; 0 disables), `settingsPath` (default
`~/.claude/settings.json`, test override), `projectsDir` override drives `@local` tests. Stale-lag is
core's global `lagThresholdSeconds` (the plugin does not duplicate it); the plugin's own liveness signal
is the pid-liveness sweep. Hook install is a `supergraph install-claude-hook --install-hook` CLI flag (owner
decision C1, default off), **not** a plugin config key — the plugin never writes settings itself.

## LOC budget (prod, guard **750**; tests excluded)

Table shows target vs **Actual** LOC (EDR strip formula: non-comment, non-blank). CLI hook-install delta
lives in `cmd/supergraph/` and is counted separately, like github's. The **Actual** column is measured
after the review-findings batch (M1/M2/S1–S7) landed; the Δ column attributes the growth over the first
measure (677).

| File (`plugins/claude/`) | Target | **Actual** | Δ | Responsibility (and what the batch added) |
|---|---:|---:|---:|---|
| `claude.go` | 110 | **128** | −25 | wiring + query funcs; +`retentionDays` config, softened privacy doc; `strOr`/`boolOr`/`intOr` moved to `plugins/internal/pluginconfig` (S2/S4/S7) |
| `keys.go` | 70 | **29** | 0 | key grammar (`session:`/`instance:`) + `issueNumber` from branch + PR# from URL |
| `store.go` | 150 | **255** | +31 | fold/dedup/enrich/get/list/instances/stale-scan; +`prune` retention sweep (S2) |
| `hook.go` | 110 | **60** | −7 | `/hook` parse+validate→reducer→emit; +`validSessionID` format gate (S3); `readErrStatus` moved to `plugins/internal/pluginconfig` |
| `reducer.go` | 100 | **34** | 0 | ported state machine shared by hook + tail |
| `tail.go` | 120 | **128** | +29 | poll-scan + JSONL parse + offset cursor + backfill/enrich + stale sweep; +capped `readLine` + retention call (S1/S2) |
| `redact.go` | 30 | **76** | 0 | whitelist projection: hook + transcript record → body-free foldInput/enrichment (comment softened, S4) |
| **Total** | **690** | **710** | +33 | guard **750** = measured 710 + 5% rounded up to a multiple of 10 (owner rule); ratcheted 790→750 after the config helpers moved to `plugins/internal/pluginconfig` (−35) |

`live` was moved to `plugins/internal/single.Ptr[Plugin]` (post-tier §A) — LOC-neutral, measured
holds at **710**, cap stays 750.

CLI delta (`cmd/supergraph/install.go`, counted separately like github's): **183** actual (was 150) — the
opt-in `install` (print-block + idempotent, now **atomic** `--install-hook` merge that refuses to
overwrite an unparseable file, M1) **and** the `claude-hook` stdin→POST forwarder (deterministic token
pick, S6). The `query --op --plugin` client change lives in `cmd/supergraph/query.go`, also separate.

- **AC-CLAUDE-LOC** guards it: a CI step counts non-comment, non-blank prod lines under
  `plugins/claude` excluding `*_test.go` and fails > **750** (same `sed`/`wc` formula as `make
  loc-github`). Measured total **710**; the guard is measured + 5% rounded up to a multiple of 10
  (**750**) per the owner rule — the Target column is the original estimate, not the guard.

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
- **S4 (claude half only)** — **AC-CLAUDE-S4-HALF**: a `claudeSessions` query filtered by `issueNumber`
  returns ≥ 1 source-tagged `ClaudeSession` row. The umbrella S4 (join over github+claude, absent-source
  `staleSince` marker) stays PRD-`@pending` until the `touching` resolver lands.

**Deferred (owner decision C3):** subagent / background (`isSidechain`, `sessionKind:bg`) sessions are
out of scope for v1 — the tail records only top-level sessions; a `@pending` AC will cover them later.

**Plugin ACs `@AC-CLAUDE-*`:**
`HOOK-INGEST` (a SessionStart→UserPromptSubmit→PreToolUse→Stop hook sequence folds `idle→working→idle`,
queryable) · `HOOK-AUTH` (on a non-loopback token-configured instance a hook POST without/with a wrong
Bearer is 401 via core middleware — no row, no envelope; the correct token passes) · `HOOK-SCHEMA` (an
unknown `hook_event_name` or a non-numeric `issue_number` → 400, nothing stored) · `STATE-MACHINE`
(`Notification` permission→`input`; the `"waiting for your input"` nag
stays `idle`; `Stop`→`idle`) · `PANE` (`TMUX_PANE` at hook time builds a `ClaudeInstance{pane,pid}`
joining to a `TmuxPane`) · `ISSUE-JOIN` (`issueNumber` from `gitBranch` + confirmed by `pr-link`, drives
S4) · `TAIL-BACKFILL` (a session with **no** hook events is discovered by the transcript tail within one
scan) · `TAIL-ENRICH` (the tail adds `gitBranch`/`model`/`prNumber`/`prUrl` the hook lacks) · `STALE`
(negative control: a session whose `pid` is dead shows `staleSince` with no `SessionEnd`) · `PRIVACY`
(negative control: after a session with prompts + tool calls, **none** of the db, the emitted envelopes
observed on a live `claudeSessionUpdated` subscription, or the `claudeSession` GraphQL result hold any
prompt/response/tool_input bytes) · `INSTALL-IDEMPOTENT` (the settings.json hook merge runs twice → no duplicate hook
entries, existing hooks untouched) · `DEDUP` (the same transition via hook **and** tail → one row, no
double-count) · `CURSOR` (the per-transcript byte offset persists across restart and resumes) ·
`ZEROCORE` (`git diff --stat core/` = 0) · `LOC` (prod LOC ≤ 700).

## Coder step plan — two file-partitioned waves (no shared-file contention)

**Wave 1 — plugin** (each file one coder task):
1. `plugins/claude/keys.go` + `store.go` + `Migrate` (one table) — key grammar + session store + pid-liveness.
2. `plugins/claude/reducer.go` — port `fold-state.sh`'s state machine to Go (pure fn: prev+event→next).
3. `plugins/claude/redact.go` — whitelist projection (the privacy guard).
4. `plugins/claude/hook.go` — `HTTPRoutes` `/hook`: payload → reducer → emit; pane/input path.
5. `plugins/claude/tail.go` — poll-scan ticker + JSONL parse + offset cursor + backfill + enrich.
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
