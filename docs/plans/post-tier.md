# Plan: post-tier follow-ups (after PR #12 / plugin/tmux merges)

Two owner-approved follow-ups that start once `plugin/tmux` (PR #12) lands on `main`.
Output shape per `~/.knowledge/modules/shared/records/principles/plan-first.md`; AC sharpness
per `~/.knowledge/modules/shared/records/principles/acceptance-criteria.md`. **Core is LOCKED —
neither follow-up touches `core/` or `server/`.**

---

## A. Shared config-helper package `plugins/internal/pluginconfig`

### Goal
Delete the copy-pasted TOML-coercion helpers duplicated across 4 plugins into one internal
package; re-ratchet each plugin's LOC gate down to reflect the removal.

### Non-goals
- No behavior change to any resolver, ingest, or config parse — pure extract-and-call.
- No new config KEYS, no schema/graphql change, no core edit.
- Not touching `plugins/template` (no such helpers there) or the fakes.

### Duplication inventory (the 4 copies + 1 hook copy)
| Plugin | File:line | Helpers (prod, gate-counted) |
|---|---|---|
| github | `plugins/github/github.go:198` `:207` `:216` `:233`; `:22` | `strOr` `boolOr` `intOr` `toInt` + `readErrStatus` (~49 LOC) |
| claude | `plugins/claude/claude.go:186` `:195` `:204`; `hook.go:~88` | `strOr` `boolOr` `intOr` + `readErrStatus` (~35 LOC) |
| peer   | `plugins/peer/peer.go:201` `:~209` | `strOr` `numOr` (float64 variant) (~22 LOC) |
| tmux   | `plugins/tmux/tmux.go:~176` `:187` `:198` | `strOr` `intOr` `strsOr` (~36 LOC) |

`subMap` (github:~) stays local (single caller). Note the two int flavors: github/claude/tmux
use `intOr → int`; peer uses `numOr → float64`. Both must exist in the shared API.

### Proposed API (`plugins/internal/pluginconfig/pluginconfig.go`)
Import path `github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig`.
```go
func Str(raw map[string]any, k, def string) string
func Int(raw map[string]any, k string, def int) int          // accepts int/int64/float64
func Num(raw map[string]any, k string, def float64) float64   // peer's staleThreshold etc.
func Bool(raw map[string]any, k string, def bool) bool
func Strs(raw map[string]any, k string) []string             // tmux idleShells
func ReadErrStatus(err error) int                            // 413 on MaxBytesError else 400
```
`intOr`'s "0 means unset → def" quirk (github) vs claude's type-switch differ subtly: adopt the
**type-switch** semantics (claude/tmux/peer) as canonical — it is the correct one; github's
`n != 0` guard is a latent bug (a legit `0` config falls back to def). Fixing it in the extract
is a Boy-Scout win; call it out in the PR so the owner sees the semantic change.
Test file: `plugins/internal/pluginconfig/pluginconfig_test.go` (table tests per helper, incl.
nil-map, wrong-type, int64/float64 coercion, MaxBytesError mapping).

### Step sequence (TDD)
1. Write `pluginconfig_test.go` red — one table per helper, asserting the canonical semantics.
2. Write `pluginconfig.go` to green. New pkg ≈ 45-50 gate-LOC.
3. Per plugin, delete the local funcs, add the import, rewrite call sites (`strOr(` →
   `cfg.Str(`, `numOr(`→`cfg.Num(`, `readErrStatus(`→`cfg.ReadErrStatus(`). Mechanical —
   `fast-coder` fodder once the API is frozen. Order: peer, tmux, claude, github.
4. `go build ./... && make test features` — no behavior delta expected.
5. Re-ratchet gates (below), commit.

### LOC gate re-ratchet (owner rule: cap = ceil(measured×1.05), **never raise**)
Formula lives in `Makefile` (`loc-<plugin>`, POSIX `[[:space:]]` strip). Numbers below are
**estimates** — coder MUST run `make loc-<p>` on the post-move tree and set the cap from the
real number; keep the current cap if `ceil(measured×1.05)` would exceed it.
| Plugin | cap before | measured before | measured after move | new cap (×1.05, round up to 10) |
|---|---|---|---|---|
| github | 1350 | 1350 | 1311 | **1350** (unchanged; 1311×1.05 rounds to 1380 > 1350, cap never rises) |
| claude | 790 | 745 | 710 | **750** |
| peer   | 700 | 665 | 644 | **680** |
| tmux   | 860 | 813 | 776 | **820** |
| internal (new) | — | — | 89 | **100** |
New package needs its own budget: added `loc-internal` (cap **100**) as a single umbrella
— **decision for owner**: recommend a single `loc-internal` gate so future `plugins/internal/*`
helpers share one budget. Wire it into the `dev-check`/CI aggregate that already runs `loc-*`.

### Singleton question — shared `plugins/internal/single.Ptr[T]`?
Three call sites: claude `var live atomic.Pointer[Plugin]` (claude.go:33), peer `var active
atomic.Pointer[Plugin]` (accessor.go:20) — **identical** atomic pattern; tmux `currentMu
sync.RWMutex + current *Plugin` (tmux.go:59) — a **different** (mutex) pattern for the same job.
**Recommendation: YES, but as a small second PR, lower conviction than the config extract.**
Reasoning: a generic `single.Ptr[T]{ Store(*T); Load() *T }` wrapping `atomic.Pointer[T]`
(a) dedups the claude/peer pair cleanly, (b) lets tmux drop its RWMutex for the same lock-free
read the other two use — removing a genuine pattern divergence, not just LOC. Cost is ~15 gate-
LOC net and one tiny generic type (~8 LOC). It clears the DRY bar (3 real copies, one already
divergent) over YAGNI. Do NOT block the config PR on it. If the owner prefers minimum surface,
skipping it is defensible — the atomic.Pointer body is 3 lines each.

### Alternatives considered (A)
- **Leave as-is (4 copies).** Rejected: the copies already carry `TODO: consolidate` comments
  (claude.go:~183, peer.go:172); owner approved the extract.
- **Put helpers in `core/`.** Rejected: core is LOCKED and these are plugin-config concerns,
  not core contract.
- **Generic `Get[T](raw,k,def T)`.** Rejected: reflection/type-switch on generic T is uglier
  than 5 named funcs and loses the int-vs-float call-site clarity.

### Risks (A)
- **Semantic drift on `intOr`**: adopting the type-switch fixes github's `0→def` bug — a real
  behavior change on `0`-valued github int config. Low blast radius (no such key today) but must
  be named in the PR, not silent (`silent-divergence`).
- **peer `Num` vs `Int`**: mixing them at a call site would truncate. Test both explicitly.
- Reversible, in-repo, no one-way doors.

### AC draft (A) — <!-- ACs ready for ac-reviewer -->
- **AC-PCFG-EXTRACT** — `grep -rn 'func strOr\|func boolOr\|func intOr\|func numOr\|func toInt\|func strsOr\|func readErrStatus' plugins/{github,claude,peer,tmux}` returns **zero** prod hits (test files exempt); every former caller compiles against `pluginconfig`. Evidence: grep output + `go build ./...`.
- **AC-PCFG-BEHAVIOR** — `make test features` passes with zero diff in any scenario outcome vs the pre-extract run. Evidence: green run.
- **AC-PCFG-INT0** — `pluginconfig.Int(map[string]any{"k":0},"k",7)` returns `0` (canonical type-switch), and a regression test pins it. Evidence: passing unit test asserting the fixed semantics.
- **AC-PCFG-COERCE** — `Int` returns the same value for `int(5)`, `int64(5)`, `float64(5)`; `Num` returns `5.0` for all three; `Str`/`Bool`/`Strs` return the default on nil-map and on wrong-type. Evidence: table test.
- **AC-PCFG-READERR** — `ReadErrStatus(&http.MaxBytesError{})` == 413, any other error == 400. Evidence: unit test.
- **AC-PCFG-LOC** — after the move, each `make loc-{github,claude,peer,tmux}` passes at its re-ratcheted cap, where each cap = the post-move `make loc-<p>` measurement × 1.05 **rounded up to the next multiple of 10** (owner rule; matches the existing 1350/700/790/800 caps), and never above the prior cap; and a dedicated `loc-internal` (or `loc-pluginconfig`) gate — cap by the same rule — guards the new package and is added as its own step in `.github/workflows/ci.yml` (there is no `loc-*` aggregate target; each gate is a separate CI step). Evidence: each `make loc-<p>` stdout + the new CI step.
- **AC-SINGLE-PTR** (if the singleton PR ships) — claude/peer/tmux singletons all route through `single.Ptr[Plugin]`; `grep -n 'sync.RWMutex' plugins/tmux/tmux.go` is empty; tests still read their own instance and a nil/unstarted ptr yields no-rows not a panic. Evidence: grep + green features.

---

## B. Spike measurements (no owner creds) → `docs/spike-results.md`

### Goal
Produce reproducible p95 numbers for two spike claims that need only local resources, and an
honest "blocked on creds" list for the rest.

### Non-goals
- No `@live` GitHub-PAT scenarios, no real cross-box co-boot (both go in the Blocked section).
- No core/schema change; measurement harness only.

### "Realistic" seed size — the PRD does NOT quantify it (§4 Assumptions is qualitative), so
define it here as the spike's operating definition (flag for owner confirm): a single operator,
handful of boxes. Seed: **10 repos, 200 open issues, 100 open PRs** (github fake), **30 claude
sessions across 8 tmux panes**, **40 tmux panes total** (real `-L` socket), **3 peers**. These
scale the joins without being fantasy; adjust if owner gives real fleet numbers.

### (a) p95 cross-plugin query latency, seeded realistic DB
**Finding — the join spans claude+tmux+peer(+core health), NOT github.** github contributes
only `extend type Subscription { checkRunUpdated }` (github.graphqls) and serves reads via
`Routes()` (REST proxy), so it is **not** a field on the unified `/graphql` Query. The ≥3-plugin
GraphQL doc uses actual fields (claude.graphqls `claudeSessions`; tmux `paneForBranch`,
`freeSlots`; peer `peers`; base `health`):
```graphql
query cross($issue: Int!, $branch: String!, $host: String) {
  claudeSessions(issueNumber: $issue) { hostId sessionId state gitBranch }
  paneForBranch(branch: $branch)      { hostId key session free staleSince }
  freeSlots(hostId: $host)            { hostId kind free paneKey }
  peers                               { hostId url lastSeenAt staleSince remoteMaxPluginLagSeconds }
  health                              { plugin state lagSeconds lastEventAt }
}
```
This is a **fan-out**, not a relational join — supergraph's actual behavior; call it out in the
results doc so the S4 "one answer" claim is read correctly (github's issue rows arrive via the
proxy path, measured separately if needed).

Seeding each plugin's SQLite via existing fakes (no creds):
- **github** — `fakegh` world API: `AddRepo(owner,repo)`, `AddIssue(...)`, `AddPR(...)`
  (`plugins/github/fakegh/world.go:65,85,97`); drive ingest through `ghstub` as the features
  suite already does.
- **claude** — POST synthetic hook events via the harness helpers `postHook`/`postHookPane`
  (`features/steps_claude_test.go:128,309`) against `/plugins/claude/hook` to seed sessions+panes.
- **tmux** — real `tmux -L sg-spike-<pid>` socket (EDR D4); script `split-window`/`new-session`
  to create the 40 panes, let the control-mode client + reconcile populate the cache.
- **peer** — `plugins/fakeremote` registered as a peer target (harness-only pkg) for the 3 rows.

Method: **N=200 paired samples** (warm cache, discard first 10) via both `supergraph query --op
cross ...` (`cmd/supergraph/query.go`) and a direct `POST /graphql`; report **p50/p95/p99** for
each path. Pass bar: p95 < 1 s warm (S2/S4 spirit).
**Deliverable form (decision): a `scripts/spike-measure.sh`** (seed → warm → 200-sample loop →
percentile print), invoked by a **`make spike-measure`** target. Script over inline Make recipe
because the seeding + timing loop is too long for a portable one-liner. Exact commands land in
`docs/spike-results.md` alongside the numbers so any box reproduces them.

### (c) Peer stale-marking across two loopback `supergraph serve` processes
Two **real** `serve` processes on `127.0.0.1:<pA>` / `:<pB>`, each `[plugins.peer]` pointing at
the other, `staleThresholdSeconds` set low (e.g. 5) via config (`peer.go` reads it through
`numOr`→`Num`). peer liveness runs a gqlgen `pluginLag` subscription; a drop sets `staleSince`
at first-failure and emits `peer.stale` on the up→down edge (`plugins/peer/liveness.go:20,57`).
Steps + exact commands (all in results doc):
1. `supergraph serve --config A.toml &` ; `supergraph serve --config B.toml &`.
2. Confirm A's `peers` query shows B `staleSince: null`, `lagSeconds` flowing.
3. `kill <pidB>`; poll A's `peers` query every 250 ms; record **t_stale** = first sample with
   B `staleSince != null` (expect ≤ `staleThreshold` + backoff, target < 30 s per F8).
4. Restart B; poll; record **t_recover** = `staleSince` back to null.
Report t_stale and t_recover with the raw poll log.

### Blocked on owner creds (grep `@live @pending` across `features/*.feature`)
10 `@live` scenarios (scenario tag lines only; a bare `grep -c` counts 18 by including comment
lines), all `@pending` until creds/boxes exist:
- **github (needs `GITHUB_TOKEN`+`GITHUB_ORG`)** — `@F2` `@F3` `@F7` `@AC-GH-FORWARD`
  `@AC-GH-NOTIFY-304` `@AC-GH-RATELOG` (`features/github.feature:194-228`).
- **peer (needs a real second box / non-loopback mesh)** — `@S2` `@F8` `@AC-PEER-AUTH`
  (`features/peer.feature:122-134`). Loopback co-boot (B.c above) covers the *mechanism*; the
  real cross-box timing stays blocked.
- **claude (needs a real tmux+Claude session on a box)** — `@AC-CLAUDE-PANE`
  (`features/claude.feature:207`).
- **umbrella (plugin-tier, need full fleet)** — prd.feature `@S1 @S2 @S4 @S5 @S6 @F1 @F4 @F5
  @F6 @F8` (`features/prd.feature`), pending until all tiers co-boot.
- **S1 issue→PR ≤15 min** and **real cross-box co-boot** — need live GitHub + ≥2 boxes: blocked.

### Alternatives considered (B)
- **Inline `make spike-measure` recipe only.** Rejected: seeding 4 plugins + a 200-sample timing
  loop is too much for a portable Make one-liner; a `scripts/` file is testable and readable.
- **Seed SQLite directly with SQL inserts.** Rejected: bypasses the real ingest/reducer path, so
  the measurement would not reflect production reads — use the fakes + hook POSTs instead.
- **Include github in the GraphQL join.** Not possible today (proxy-only); measure its proxy read
  separately rather than pretend it is a Query field.

### Risks (B)
- **"Realistic" is self-defined** — if the owner's real fleet is bigger, p95 may not hold; the
  seed numbers are flagged for confirmation, not assumed.
- **Loopback ≠ cross-box** for peer: loopback removes mesh latency, so t_stale is a *lower*
  bound; state that explicitly so the number is not read as the real-box F8 result.
- **tmux real socket** on the CI/dev box must be torn down per run (`-L sg-spike-<pid>`) to avoid
  leaking sessions (`clean-up`).

### AC draft (B) — <!-- ACs ready for ac-reviewer -->
- **AC-SPIKE-LATENCY** — `docs/spike-results.md` reports p50/p95/p99 over N≥190 warm paired
  samples for the cross-plugin doc, for both `supergraph query` and direct `POST /graphql`, on
  the seeded DB (10 repos/200 issues/100 PRs/30 sessions/40 panes/3 peers); p95 stated against
  the <1 s bar. Evidence: `make spike-measure` output + committed results doc.
- **AC-SPIKE-REPRO** — `make spike-measure` (calling `scripts/spike-measure.sh`) runs green from
  a clean checkout with no owner creds and prints the percentiles; the exact commands are in the
  results doc. Evidence: second-run output matching within noise.
- **AC-SPIKE-JOIN-HONEST** — the results doc states the query is a claude+tmux+peer+health
  fan-out and that github reads via the proxy (not the unified Query). Evidence: doc section.
- **AC-SPIKE-PEER-STALE** — two loopback `serve` procs: after `kill B`, A's `peers` shows
  `staleSince != null` within `staleThreshold`+backoff (target <30 s) with **no peer-of-peer
  rows**; after restart, `staleSince` returns to null. Both times + raw poll log in the doc.
  Evidence: poll log + `peers` query screenshots.
- **AC-SPIKE-BLOCKED** — the doc's "Blocked on owner creds" section lists every `@live @pending`
  scenario from `features/*.feature` with its required credential/resource, and the number of
  listed `@live` scenarios equals `grep -rn '@live' features/*.feature | grep -v '#' | wc -l`
  (scenario tag lines only, comments excluded — currently **10**: github 6, peer 3, claude 1).
  Note: a bare `grep -c '@live'` returns 18 because it counts comment lines, so it is NOT the
  count to match. The @pending-but-not-@live umbrella scenarios (`prd.feature` S1/S2/S4/S5/S6,
  F1/F4/F5/F6/F8 and the cross-box S1) are listed in a separate sub-section and are NOT part of
  the @live count. Evidence: the `@live` grep above + doc.

---

## Handoff
- ACs ready for ac-reviewer (see §A AC draft and §B AC draft above).
- Implementation → `coder` for the pluginconfig extract + spike harness/design; the mechanical
  call-site rewrites (step A.3) → `fast-coder` once the API is frozen, per
  `~/.knowledge/modules/shared/records/model-selection.md`.
- Two open owner decisions flagged inline: (1) `loc-internal` umbrella gate vs per-pkg gate;
  (2) ship the `single.Ptr[T]` singleton PR — recommended yes, low priority.
