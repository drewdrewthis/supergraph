# EDR: spike-measure — no-creds measurement harness for post-tier §B

Engineering Design Record for `scripts/spike-measure.sh` + `scripts/spikeharness/`. Owns the
decisions the [plan](../plans/post-tier.md) §B leaves to the harness. Act-doc: decisions, not prose.

**Goal.** Produce reproducible p95 numbers for two spike claims that need only local resources —
(a) the `AC-GHQ-P95` cross-plugin query latency, (c) the two-box peer stale-marking + recovery —
and regenerate [`../spike-results.md`](../spike-results.md) with the real numbers. No PAT, no
second box, no Claude install. Run it with `make spike-measure`.

## D1 — Seed size ("realistic", self-defined)
The PRD §4 does not quantify a fleet, so the spike defines it: **10 repos, 200 open issues, 100
open PRs** (github), **30 claude sessions**, **40 tmux panes** (real socket), **1 peer**. These
scale the joins without being fantasy; flagged for owner confirm. Env-overridable (`REPOS`,
`ISSUES`, `PRS`, `SESSIONS`, `N`, …). The seed is **concentrated in repo `o/r`** and every pane
is rooted at one `issue5/spike-core` worktree — a conservative (heavier) upper bound on the
issue-5 join cardinality, not the average case.

## D2 — Sample count N + two latency paths
**N = 200 measured samples after a 20-sample warmup discard** (the brief's N≥190; the tail needs
the extra samples). Both transport paths for the *same* AC-GHQ-P95 query are measured and reported
separately (AC-SPIKE-LATENCY): `measure` POSTs the query file to `/graphql` (the graph's own
latency); `measure-cli` forks `supergraph query '<same query>' --endpoint …` per sample, so its
p50/p95/p99 **include a process spawn + cobra startup** on top of the read — reported as the
operator's real CLI latency, never conflated with the HTTP number. Both drop the warmup and report
p50/p95/p99. Bar: **p95 < 1 s** (PRD S2/S4 warm) on the HTTP path; the CLI path is reported for
information. The `AC-GHQ-P95` feature scenario itself only asks for 20 samples; the spike
over-samples to 200 for a tighter tail.

## D3 — Staleness method (event-driven WS drop, not a timer)
Per [`peer.md`](peer.md) D7, peer liveness **is** an active `pluginLag` WS subscription: a drop /
dial-fail / 401 pins `staleSince` at first-failure immediately; there is no local age-poll.
The harness therefore **SIGKILLs** B (RST closes the socket) — a bare **SIGSTOP would freeze B
without closing the TCP socket**, so A's established subscription would never drop and staleness
would never be detected. `pollpeer` reads A's `peers { staleSince lastSeenAt }` every 200 ms and
records elapsed-ms to the first `staleSince != null` (t_stale) and back to null (t_recover).

**Kill-anchored clock (AC-SPIKE-PEER-STALE).** The stale clock is captured at the `kill -9` call
itself (a portable `now_ms` python stamp, before `pollpeer` execs), so `t_stale_kill_ms` is the
full detect latency F8 bounds; `pollpeer`'s own `elapsed_ms` (from its first poll) is reported
alongside as the smaller window. The verdict is computed against the **kill-anchored** figure.
`pollpeer --log <file>` writes one timestamped line per 200 ms sample; the script embeds both raw
logs (kill→stale, restart→recovered) as code blocks in the results doc.

**No peer-of-peer rows (F8 invariant).** After A mirrors B, `peercheck` asserts A's `peers` lists
only its directly-configured peer (`boxB`) and A's `peer_nodes` mirror holds no row whose
peer_host or origin `@host` key is a third host — printing `verdict=PASS|FAIL`, embedded in the
results doc's (c) section.

## D4 — tmux via a real private socket
The tmux plugin is exercised through a real `tmux -L sgmeasure` server (not seeded SQLite), so
the reconcile ingest path runs for real. github + claude caches ARE seeded by direct SQLite
writes — the exact shape `features/steps_githubquery_test.go` uses for the very AC measured — so
the read-path latency is faithful while avoiding a live warm. fakegh is still hosted and wired as
`[plugins.github].baseURL` so the plugin is non-dormant and its boot reconcile succeeds.

## D5 — Two loopback processes stand in for two boxes
Part (c) boots **two real `supergraph serve` processes** on free loopback ports, each
`[[plugins.peer.peers]]` pointing at the other. This proves the **mechanism** (subscription drop →
`markStale` → backoff reconnect → `markSeen`) end to end without a second machine. **Loopback is a
lower bound:** it removes real mesh latency, so the real cross-box F8 timing will be larger — that
number stays blocked (`@F8`, live). `ingress = "tunnel"` on the github plugin means **no
`gh webhook forward`/ghstub child is spawned** (that child is `ingress = "forward"` only), so the
run leaves no forwarder to orphan.

## D6 — Teardown
A single `trap teardown EXIT INT TERM` kills both serves + fakegh (SIGTERM then SIGKILL), runs
`tmux -L sgmeasure kill-server`, a `$WORK`-scoped `pkill -f "$WORK"` (belt-and-suspenders for any
temp-dir child), then `rm -rf "$WORK"`. A final pgrep/tmux-list leftover check warns if anything
survived. The `features/spike-measure.feature` `@slow` scenario asserts a clean pgrep after a run.

## D7 — Output goes to a standalone temp file by default; publish is explicit
A bare `scripts/spike-measure.sh` (and the `@slow` feature run) writes the results doc to a
`mktemp` file (`sg-spike-results-*`) **outside `$WORK`** — outside because the teardown
`rm -rf "$WORK"` would otherwise delete it before the caller could read it — so a measurement run
**never dirties the committed doc** (`git status --porcelain docs/spike-results.md` stays empty —
the `@slow` scenario asserts this). Publishing into the repo is opt-in: `make spike-measure-publish`
(or `OUT=docs/spike-results.md scripts/spike-measure.sh`) points `OUT` at the committed file. The
SUMMARY records `results_doc=<path>` so the `@slow` scenario reads the generated temp file, asserts
its sections + PASS/FAIL tokens, then removes it — the run never touches the repo.

## Deviations
- **github is a first-class Query field (AC-SPIKE-JOIN-HONEST).** The plan's §B premise — github
  serves reads via a REST proxy and is *not* a field on the unified `/graphql` Query — is stale.
  `github-query` added typed core Query fields (`Query.issue`, `pullRequest`, `issuesForRepo`) plus
  the `issue.tmuxPanes`/`claudeSessions` join, so the `AC-GHQ-P95` query IS the github→tmux→claude
  fan-out over the unified graph (the primary, measured on both HTTP and CLI paths); the plan's
  original claude/tmux/peer/health fan-out is kept as the sibling query. No proxy is involved.
- **`@live` count is 11, not the plan's 10** — `github-query` added `@AC-GHQ-LIVE-WARM`
  (github 6→7; peer 3; claude 1).
