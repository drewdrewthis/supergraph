#!/usr/bin/env bash
# spike-measure.sh — reproducible, no-creds spike measurements for docs/plans/post-tier.md §B.
#
# Measures (a) the AC-GHQ-P95 cross-plugin query (issue -> tmuxPanes + claudeSessions)
# latency over a seeded, warm cache, and (c) the two-box peer stale-marking + recovery
# times across two loopback `supergraph serve` processes. It uses only local resources:
# a fresh binary built to a temp dir, free loopback ports, a private `tmux -L sgmeasure`
# socket, the in-repo plugins/github/fakegh upstream (hosted by scripts/spikeharness),
# and direct SQLite cache seeds in the shape features/steps_githubquery_test.go uses.
# Every process, the tmux socket, and the temp dir are torn down on exit.
#
# Output: a machine-readable SUMMARY block on stdout and a refreshed docs/spike-results.md.
# See docs/edr/spike-measure.md for the decisions behind seed size, N, and method.
set -euo pipefail

# ---- config (the spike's operating definition of "realistic"; see the EDR) ----
REPOS=${REPOS:-10}
ISSUES=${ISSUES:-200}
PRS=${PRS:-100}
SESSIONS=${SESSIONS:-30}
TMUX_SESSIONS=${TMUX_SESSIONS:-8}
PANES_PER_SESSION=${PANES_PER_SESSION:-5} # 8 x 5 = 40 panes total
N=${N:-200}                               # measured samples per path (warmup excluded)
WARMUP=${WARMUP:-20}
STALE_THRESHOLD=${STALE_THRESHOLD:-5}
BACKOFF_MAX=${BACKOFF_MAX:-2}
SOCKET=sgmeasure

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/sg-spike-XXXXXX")
BIN="$WORK/supergraph"
HARNESS="$WORK/spikeharness"
# Default output is a standalone temp file (NOT under $WORK, which teardown removes), so
# a bare run — and the @slow feature — produces a readable results doc without dirtying
# the committed one. `make spike-measure-publish` (or OUT=docs/spike-results.md) points
# OUT at the repo doc instead. See docs/edr/spike-measure.md D7.
RESULTS="${OUT:-$(mktemp "${TMPDIR:-/tmp}/sg-spike-results-XXXXXX")}"

# now_ms prints a portable wall-clock milliseconds stamp (BSD `date` has no %3N).
now_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }

PIDS=""        # space-separated child pids to kill on teardown
FAKEGH_PID=""

# ---- teardown (trap): kill every child, the tmux socket, and the temp dir ----
teardown() {
  local ec=$?
  set +e
  for p in $PIDS $FAKEGH_PID; do
    [ -n "$p" ] && kill "$p" 2>/dev/null
  done
  # give serves a moment to exit on SIGTERM, then hard-kill stragglers
  sleep 1
  for p in $PIDS $FAKEGH_PID; do
    [ -n "$p" ] && kill -9 "$p" 2>/dev/null
  done
  tmux -L "$SOCKET" kill-server 2>/dev/null
  # Belt-and-suspenders: reap any child rooted in the temp dir (e.g. a `gh webhook
  # forward`/ghstub supervisor if the github ingress is ever flipped to "forward";
  # the spike wires ingress="tunnel", which spawns none). Scoped to $WORK so it can
  # never touch another session's processes.
  pkill -f "$WORK" 2>/dev/null
  rm -rf "$WORK"
  # leftover check
  local leftover
  leftover=$( { pgrep -f "$WORK"; tmux -L "$SOCKET" list-sessions 2>/dev/null; } 2>/dev/null | wc -l | tr -d ' ')
  if [ "$leftover" != "0" ]; then
    echo "WARN: $leftover leftover process/session after teardown" >&2
  fi
  exit $ec
}
trap teardown EXIT INT TERM

log() { echo "[spike] $*" >&2; }

freeport() { "$HARNESS" freeport; }

# wait_health URL — block until GET URL/health returns 200 (max ~15s).
wait_health() {
  local url=$1
  for _ in $(seq 1 150); do
    if curl -fsS "$url/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  echo "health never came up at $url" >&2
  return 1
}

# ---- build ----
log "building supergraph binary + spikeharness into $WORK"
( cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/supergraph )
( cd "$REPO_ROOT" && go build -o "$HARNESS" ./scripts/spikeharness )
command -v tmux >/dev/null || { echo "tmux not on PATH" >&2; exit 1; }

GO_VERSION=$(go version | awk '{print $3}')
COMMIT=$(cd "$REPO_ROOT" && git rev-parse --short HEAD)
OS_DESC=$(uname -srm)

# =====================================================================================
# Part (a): p95 of the AC-GHQ-P95 cross-plugin query on a seeded, warm cache
# =====================================================================================
log "part (a): seeding + measuring the cross-plugin query"

# A real git worktree on the queried branch so the real tmux panes derive to issue 5.
GITDIR="$WORK/branch-issue5"
mkdir -p "$GITDIR"
( cd "$GITDIR" && git init -q && git config user.email spike@local && git config user.name spike \
    && git commit -q --allow-empty -m init && git checkout -q -b issue5/spike-core )

# Real tmux server on the private socket: TMUX_SESSIONS sessions rooted at the issue5
# worktree, PANES_PER_SESSION panes each -> the tmux plugin reconcile ingests them and
# every pane attaches to issue 5's branch.
tmux -L "$SOCKET" kill-server 2>/dev/null || true
s=0
while [ "$s" -lt "$TMUX_SESSIONS" ]; do
  tmux -L "$SOCKET" new-session -d -s "spk$s" -c "$GITDIR"
  p=1
  while [ "$p" -lt "$PANES_PER_SESSION" ]; do
    tmux -L "$SOCKET" split-window -t "spk$s" -c "$GITDIR"
    tmux -L "$SOCKET" select-layout -t "spk$s" tiled >/dev/null 2>&1 || true
    p=$((p + 1))
  done
  s=$((s + 1))
done
PANE_COUNT=$(tmux -L "$SOCKET" list-panes -a | wc -l | tr -d ' ')
log "tmux -L $SOCKET has $PANE_COUNT panes"

# fakegh upstream (wired so [plugins.github] is non-dormant and reconcile succeeds).
FAKEGH_OUT="$WORK/fakegh.out"
"$HARNESS" fakegh --repos "$REPOS" --issues "$ISSUES" --prs "$PRS" >"$FAKEGH_OUT" 2>&1 &
FAKEGH_PID=$!
for _ in $(seq 1 100); do grep -q '^URL=' "$FAKEGH_OUT" && break; sleep 0.05; done
FAKEGH_URL=$(sed -n 's/^URL=//p' "$FAKEGH_OUT")
[ -n "$FAKEGH_URL" ] || { echo "fakegh did not report a URL" >&2; cat "$FAKEGH_OUT" >&2; exit 1; }
log "fakegh at $FAKEGH_URL"

DATADIR_A="$WORK/data-a"
mkdir -p "$DATADIR_A"
LISTEN_A=$(freeport)
CFG_A="$WORK/a.toml"
cat >"$CFG_A" <<EOF
hostId = "spikeA"
listen = "$LISTEN_A"
dataDir = "$DATADIR_A"
lagThresholdSeconds = 30.000000
[plugins.github]
token = "spike-token"
baseURL = "$FAKEGH_URL"
graphqlURL = "$FAKEGH_URL/graphql"
ingress = "tunnel"
reconcileIntervalSeconds = 3600
notifications = false
[plugins.github.ttl]
issue = 3600
pr = 3600
[plugins.claude]
projectsDir = "$DATADIR_A/projects"
settingsPath = "$DATADIR_A/settings.json"
scanIntervalSeconds = 3600
retentionDays = 3650
[plugins.tmux]
socket = "$SOCKET"
eventSource = "poll"
reconcileIntervalSeconds = 2
EOF
mkdir -p "$DATADIR_A/projects"

"$BIN" --config "$CFG_A" serve >"$WORK/serve-a.log" 2>&1 &
PIDS="$PIDS $!"
wait_health "http://$LISTEN_A"

# Seed the github + claude caches directly (Migrate created the tables at boot).
"$HARNESS" seed-github --db "$DATADIR_A/github.db" --issues "$ISSUES" --prs "$PRS" >&2
"$HARNESS" seed-claude --db "$DATADIR_A/claude.db" --sessions "$SESSIONS" >&2

# Build a JSON request body {"query": <file contents>} once, reused by curl below.
qbody() { # $1=query-file $2=out-json
  python3 -c 'import json,sys;json.dump({"query":open(sys.argv[1]).read()},open(sys.argv[2],"w"))' "$1" "$2"
}
PRIMARY_Q="$WORK/primary.graphql"
cat >"$PRIMARY_Q" <<'EOF'
{ issue(key: "issue:o/r#5") { number tmuxPanes { key } claudeSessions { sessionId } } }
EOF
PRIMARY_BODY="$WORK/primary-body.json"
qbody "$PRIMARY_Q" "$PRIMARY_BODY"

# Wait until the tmux reconcile has populated the join for issue 5 (panes visible).
JOINED=0
for _ in $(seq 1 60); do
  n=$(curl -fsS -XPOST "http://$LISTEN_A/graphql" -H 'content-type: application/json' \
        --data @"$PRIMARY_BODY" 2>/dev/null | tr -d ' ' | grep -o '"key":"pane:' | wc -l | tr -d ' ' || true)
  if [ "${n:-0}" -gt 0 ]; then JOINED=$n; break; fi
  sleep 0.5
done
log "issue 5 join returns $JOINED panes"

# Sibling fan-out query that also touches claude + tmux + peer + health resolvers.
SIBLING_Q="$WORK/sibling.graphql"
cat >"$SIBLING_Q" <<'EOF'
{ claudeSessions(issueNumber: 5) { sessionId } paneForBranch(branch: "issue5/spike-core") { key } freeSlots { paneKey } peers { hostId } health { plugin state } }
EOF

# Two paths for the SAME primary query (AC-SPIKE-LATENCY): a direct POST /graphql, and
# the `supergraph query` CLI (which adds a process spawn per sample). Report separately.
MEASURE_PRIMARY=$("$HARNESS" measure --url "http://$LISTEN_A/graphql" --query "$PRIMARY_Q" --n "$N" --warmup "$WARMUP" --label ghq-p95-http)
MEASURE_PRIMARY_CLI=$("$HARNESS" measure-cli --bin "$BIN" --url "http://$LISTEN_A/graphql" --query "$PRIMARY_Q" --n "$N" --warmup "$WARMUP" --label ghq-p95-cli)
MEASURE_SIBLING=$("$HARNESS" measure --url "http://$LISTEN_A/graphql" --query "$SIBLING_Q" --n "$N" --warmup "$WARMUP" --label fanout-claude-tmux-peer-health)
echo "$MEASURE_PRIMARY" | grep '^MEASURE ' >"$WORK/m-primary.json"
echo "$MEASURE_PRIMARY_CLI" | grep '^MEASURE ' >"$WORK/m-primary-cli.json"
echo "$MEASURE_SIBLING" | grep '^MEASURE ' >"$WORK/m-sibling.json"
P_PRIMARY=$(sed 's/^MEASURE //' "$WORK/m-primary.json")
P_PRIMARY_CLI=$(sed 's/^MEASURE //' "$WORK/m-primary-cli.json")
P_SIBLING=$(sed 's/^MEASURE //' "$WORK/m-sibling.json")

# Tear down part (a) processes before part (c) (keeps the two-box run isolated).
# shellcheck disable=SC2086 # PIDS is an intentionally word-split list of child pids
kill $PIDS $FAKEGH_PID 2>/dev/null || true
sleep 1
tmux -L "$SOCKET" kill-server 2>/dev/null || true
PIDS=""; FAKEGH_PID=""

# =====================================================================================
# Part (c): peer stale-marking + recovery across two loopback serve processes
# =====================================================================================
log "part (c): two-box peer stale/recovery"

LISTEN_PA=$(freeport)
LISTEN_PB=$(freeport)
DATA_PA="$WORK/data-pa"; DATA_PB="$WORK/data-pb"
mkdir -p "$DATA_PA" "$DATA_PB"

write_peer_cfg() { # $1=path $2=hostid $3=listen $4=datadir $5=peerhost $6=peerurl
  cat >"$1" <<EOF
hostId = "$2"
listen = "$3"
dataDir = "$4"
lagThresholdSeconds = 30.000000
[plugins.template]
intervalSeconds = 1
[plugins.peer]
staleThresholdSeconds = $STALE_THRESHOLD.000000
backoffMaxSeconds = $BACKOFF_MAX.000000
[[plugins.peer.peers]]
hostId = "$5"
url = "$6"
token = ""
EOF
}
CFG_PA="$WORK/pa.toml"; CFG_PB="$WORK/pb.toml"
write_peer_cfg "$CFG_PA" boxA "$LISTEN_PA" "$DATA_PA" boxB "http://$LISTEN_PB"
write_peer_cfg "$CFG_PB" boxB "$LISTEN_PB" "$DATA_PB" boxA "http://$LISTEN_PA"

"$BIN" --config "$CFG_PA" serve >"$WORK/serve-pa.log" 2>&1 &
PID_A=$!; PIDS="$PIDS $PID_A"
"$BIN" --config "$CFG_PB" serve >"$WORK/serve-pb.log" 2>&1 &
PID_B=$!; PIDS="$PIDS $PID_B"
wait_health "http://$LISTEN_PA"
wait_health "http://$LISTEN_PB"

# A must first see B live (lastSeenAt flowing, staleSince null) before we kill B.
"$HARNESS" pollpeer --url "http://$LISTEN_PA/graphql" --host boxB --until live --interval-ms 200 --timeout-s 15 >&2

# Anchor the stale clock at the kill call itself (before pollpeer even execs), so the
# reported figure includes A's whole detect latency, not just pollpeer's polling window.
STALE_LOG="$WORK/poll-stale.log"
RECOVER_LOG="$WORK/poll-recover.log"

# Kill B (SIGKILL: an established WS drops on RST; a bare SIGSTOP would NOT drop it,
# so the liveness subscription would never notice — see the EDR).
log "killing boxB ($PID_B)"
T_KILL_MS=$(now_ms)
kill -9 "$PID_B" 2>/dev/null || true
STALE_LINE=$("$HARNESS" pollpeer --url "http://$LISTEN_PA/graphql" --host boxB --until stale --interval-ms 200 --timeout-s 40 --log "$STALE_LOG")
T_STALE_DETECT_MS=$(now_ms)
echo "$STALE_LINE" >&2
T_STALE_MS=$(echo "$STALE_LINE" | sed -n 's/.*elapsed_ms=\([0-9]*\).*/\1/p')       # pollpeer's own window
T_STALE_KILL_MS=$((T_STALE_DETECT_MS - T_KILL_MS))                                  # kill-anchored

# no-peer-of-peer assertion (PRD F8): A's peers must list only its direct peer boxB and
# A's mirror must hold no third-host row. Prints PEERCHECK ... verdict=PASS|FAIL.
PEERCHECK_LINE=$("$HARNESS" peercheck --url "http://$LISTEN_PA/graphql" --allow boxB --db "$DATA_PA/peer.db")
echo "$PEERCHECK_LINE" >&2
PEER_OF_PEER_VERDICT=$(echo "$PEERCHECK_LINE" | sed -n 's/.*verdict=\([A-Z]*\).*/\1/p')

# Restart B and measure recovery (staleSince back to null on the backoff reconnect).
log "restarting boxB"
"$BIN" --config "$CFG_PB" serve >>"$WORK/serve-pb.log" 2>&1 &
PID_B=$!; PIDS="$PIDS $PID_B"
wait_health "http://$LISTEN_PB"
RECOVER_LINE=$("$HARNESS" pollpeer --url "http://$LISTEN_PA/graphql" --host boxB --until live --interval-ms 200 --timeout-s 40 --log "$RECOVER_LOG")
echo "$RECOVER_LINE" >&2
T_RECOVER_MS=$(echo "$RECOVER_LINE" | sed -n 's/.*elapsed_ms=\([0-9]*\).*/\1/p')

# =====================================================================================
# Machine-readable summary
# =====================================================================================
LIVE_COUNT=$(grep -rn '@live' "$REPO_ROOT"/features/*.feature | grep -vc '#' | tr -d ' ')

cat <<EOF
==== SPIKE SUMMARY ====
os=$OS_DESC
go=$GO_VERSION
commit=$COMMIT
seed=repos:$REPOS,issues:$ISSUES,prs:$PRS,claudeSessions:$SESSIONS,tmuxPanes:$PANE_COUNT,peers:1
join_panes_for_issue5=$JOINED
primary=$P_PRIMARY
primary_cli=$P_PRIMARY_CLI
sibling=$P_SIBLING
t_stale_ms=$T_STALE_MS
t_stale_kill_ms=$T_STALE_KILL_MS
t_recover_ms=$T_RECOVER_MS
peer_of_peer=$PEER_OF_PEER_VERDICT
live_pending_scenarios=$LIVE_COUNT
results_doc=$RESULTS
=======================
EOF

# =====================================================================================
# Refresh docs/spike-results.md with the real numbers
# =====================================================================================
extract() { echo "$1" | python3 -c "import json,sys;d=json.load(sys.stdin);print(d['$2'])"; }
PP50=$(extract "$P_PRIMARY" p50_ms); PP95=$(extract "$P_PRIMARY" p95_ms); PP99=$(extract "$P_PRIMARY" p99_ms)
PVERD=$(extract "$P_PRIMARY" verdict); PN=$(extract "$P_PRIMARY" n)
CP50=$(extract "$P_PRIMARY_CLI" p50_ms); CP95=$(extract "$P_PRIMARY_CLI" p95_ms); CP99=$(extract "$P_PRIMARY_CLI" p99_ms)
CVERD=$(extract "$P_PRIMARY_CLI" verdict); CN=$(extract "$P_PRIMARY_CLI" n)
SP50=$(extract "$P_SIBLING" p50_ms); SP95=$(extract "$P_SIBLING" p95_ms); SP99=$(extract "$P_SIBLING" p99_ms)
SVERD=$(extract "$P_SIBLING" verdict); SN=$(extract "$P_SIBLING" n)
# Stale verdict is anchored at the kill call (the figure F8 actually bounds).
STALE_VERDICT=PASS; [ "${T_STALE_KILL_MS:-999999}" -lt 30000 ] || STALE_VERDICT=FAIL
POP_VERDICT=${PEER_OF_PEER_VERDICT:-FAIL}
STALE_LOG_BODY=$(cat "$STALE_LOG" 2>/dev/null || echo "(no stale poll log captured)")
RECOVER_LOG_BODY=$(cat "$RECOVER_LOG" 2>/dev/null || echo "(no recovery poll log captured)")

cat >"$RESULTS" <<EOF
# Spike results — post-tier §B (no owner credentials)

Generated by \`scripts/spike-measure.sh\`. Every number below is a real measurement from the run
recorded in **Environment**. A bare run writes to a throwaway temp dir (no repo churn);
\`make spike-measure-publish\` (or \`OUT=docs/spike-results.md scripts/spike-measure.sh\`)
regenerates **this** committed file. Decisions behind seed size, sample count, both latency
paths, and the staleness method live in \`docs/edr/spike-measure.md\`.

## Environment
| Field | Value |
|---|---|
| OS | $OS_DESC |
| Go | $GO_VERSION |
| Commit | $COMMIT |
| Seed | $REPOS repos, $ISSUES issues, $PRS PRs, $SESSIONS claude sessions, $PANE_COUNT tmux panes (real \`-L $SOCKET\` socket), 1 peer |
| Panes returned by the issue-5 join | $JOINED |

## Reproduce
\`\`\`sh
make spike-measure          # run only (writes to a temp dir, no repo churn)
make spike-measure-publish  # run + overwrite this committed doc
\`\`\`
The script builds the binary to a temp dir, opens free loopback ports, creates a private
\`tmux -L $SOCKET\` server, hosts \`plugins/github/fakegh\` via \`scripts/spikeharness\`, seeds
the github + claude caches directly (the shape \`features/steps_githubquery_test.go\` uses),
measures over HTTP, then boots two \`supergraph serve\` peers to time the stale/recover
transition. No PAT, no second box, no Claude install.

## (a) Cross-plugin query latency (warm cache)

The measured query is the exact **AC-GHQ-P95** doc from \`features/github-query.feature\`:
\`\`\`graphql
{ issue(key: "issue:o/r#5") { number tmuxPanes { key } claudeSessions { sessionId } } }
\`\`\`
\`issue\` **is a unified core Query field** (\`Query.issue\`, served from the github plugin's
cache) — the schema exposes it directly, so this is one graph read, not a REST-proxy call. Its
\`tmuxPanes\`/\`claudeSessions\` fields fan out to the tmux and claude plugins
(\`graph/github_map.go\`) over one shared branch-derivation (\`internal/issuekey\`), reading all
three plugins' SQLite caches with zero upstream call (S4's "one answer, not five lookups").

Both transport paths for that same query are measured over N=$PN warm samples (20-sample warmup
excluded): a direct \`POST /graphql\`, and the \`supergraph query\` CLI. **The CLI figure includes
a fresh process spawn per sample** (binary exec + cobra startup), so it is strictly slower than
the HTTP path and is reported only for the operator's real command-line latency.

| Path | Query | N | p50 (ms) | p95 (ms) | p99 (ms) | Bar | Verdict |
|---|---|---|---|---|---|---|---|
| direct \`POST /graphql\` | AC-GHQ-P95 join (issue → tmuxPanes + claudeSessions) | $PN | $PP50 | $PP95 | $PP99 | p95 < 1000ms | **$PVERD** |
| \`supergraph query\` CLI (incl. process spawn) | same query | $CN | $CP50 | $CP95 | $CP99 | p95 < 1000ms | **$CVERD** |
| direct \`POST /graphql\` | Sibling fan-out (claudeSessions + paneForBranch + freeSlots + peers + health) | $SN | $SP50 | $SP95 | $SP99 | p95 < 1000ms | **$SVERD** |

**Interpretation.** On the direct HTTP path both the AC-GHQ-P95 join and the sibling fan-out are
well under the PRD S2/S4 warm bar of 1 s at p95. The CLI path adds per-invocation spawn overhead
on top of the same graph read, so its percentiles are higher; treat it as the end-user CLI
latency, not the graph's. The sibling query confirms the tmux + claude + peer + health resolvers
are each sub-second too; \`peers\` returns \`[]\` in the single-box part-(a) server (no peer
configured) yet still exercises the resolver round-trip.

## (c) Peer stale-marking + recovery (two loopback serve processes)

Two real \`supergraph serve\` processes on loopback, each \`[[plugins.peer.peers]]\` pointing
at the other, \`staleThresholdSeconds = $STALE_THRESHOLD\`, \`backoffMaxSeconds = $BACKOFF_MAX\`.
A's peer plugin holds a \`pluginLag\` websocket subscription to B; killing B drops the socket
and \`markStale\` records \`staleSince\` at first-failure; restarting B reconnects on backoff
and \`markSeen\` clears it (\`plugins/peer/liveness.go\`, \`store.go\`).

| Transition | Measured | Target (PRD F8) | Verdict |
|---|---|---|---|
| kill B → A marks B \`staleSince != null\` (**kill-anchored**) | ${T_STALE_KILL_MS} ms | < 30 000 ms | **$STALE_VERDICT** |
| kill B → A marks B \`staleSince != null\` (pollpeer's own window) | ${T_STALE_MS} ms | < 30 000 ms | — |
| restart B → A clears \`staleSince\` (recovery) | ${T_RECOVER_MS} ms | (no PRD bar) | — |

The **kill-anchored** figure starts the clock at the \`kill -9\` call itself (before \`pollpeer\`
even execs), so it is the full detect latency F8 bounds; the second row is \`pollpeer\`'s own
elapsed window (from its first poll), always slightly smaller.

**No peer-of-peer rows (F8 invariant): $POP_VERDICT.** After A mirrors B, A's \`peers\` lists only
its directly-configured peer (\`boxB\`) and A's \`peer_nodes\` mirror holds no row keyed to a third
host — so B's own peers never leak into A's view. Raw check:
\`\`\`
$PEERCHECK_LINE
\`\`\`

**Raw poll log — kill → stale** (\`pollpeer --until stale\`, one line per 200 ms sample):
\`\`\`
$STALE_LOG_BODY
\`\`\`

**Raw poll log — restart → recovered** (\`pollpeer --until live\`):
\`\`\`
$RECOVER_LOG_BODY
\`\`\`

**Interpretation.** Stale marking is driven by the subscription drop, not a timer, so it is
fast (well under F8's 30 s) and does not depend on \`staleThreshold\` — that threshold governs
the \`pluginLag\` subscription's own lag reporting, not the drop detection. **Loopback is a
lower bound:** it removes real mesh latency, so a real cross-box F8 number will be larger; the
loopback run proves the *mechanism*, not the production timing (that stays blocked, below).

## Deviations from the plan (§B) and from the "realistic" seed
- **github is a first-class Query field now (AC-SPIKE-JOIN-HONEST).** The plan's §B premise —
  "github is not a field on the unified \`/graphql\` Query, it serves reads via a REST proxy" — is
  stale. The \`github-query\` branch added typed core Query fields (\`Query.issue\`,
  \`pullRequest\`, \`issuesForRepo\`) plus the \`issue.tmuxPanes\`/\`claudeSessions\` join, so the
  AC-GHQ-P95 query IS the github→tmux→claude fan-out over the unified graph — no proxy involved.
  It is measured as the primary; the plan's original claude/tmux/peer/health fan-out is kept as
  the sibling query.
- **Caches seeded by direct SQLite writes**, not by warming through fakegh. This is the exact
  mechanism \`features/steps_githubquery_test.go\` uses for the very AC being measured, so the
  read-path latency is faithful. fakegh is still hosted and wired as the upstream (so the
  plugin is non-dormant and its boot reconcile succeeds); the \`@live\` parity path is the
  fakegh/real-GitHub warm, listed as blocked below.
- **Seed concentrated in repo \`o/r\`.** All $ISSUES issues and $PRS PRs live in the queried
  repo so the join's per-repo cached-PR scan sees the full PR set — a conservative (heavier)
  upper bound rather than spreading across the $REPOS repos.
- **tmux realism via one issue-5 worktree.** The $PANE_COUNT real panes are rooted at a single
  git worktree checked out on \`issue5/spike-core\`, so every pane derives to issue 5 and feeds
  the join. This over-states the join cardinality for one issue (heavier = conservative).
- **SIGKILL, not SIGSTOP, triggers staleness.** A SIGSTOP freezes B without closing its TCP
  socket, so A's established \`pluginLag\` subscription never drops and staleness is never
  detected. Killing B (RST/close) is the correct drop trigger and is what F8 models.

## Blocked on owner credentials
$LIVE_COUNT \`@live @pending\` scenarios across \`features/*.feature\` need credentials/boxes
that this no-creds spike cannot supply. Count matches
\`grep -rn '@live' features/*.feature | grep -v '#' | wc -l\` = **$LIVE_COUNT**. (A bare
\`grep -c '@live'\` returns more because it counts comment lines.)

**Note:** the plan's §B estimated 10 (github 6, peer 3, claude 1). The current tree has
**$LIVE_COUNT** because the \`github-query\` branch added \`@AC-GHQ-LIVE-WARM\`, so github is now 7.

Every \`@live @pending\` scenario, its feature file, and what it needs:

| Scenario | Plugin / feature file | Needs |
|---|---|---|
| \`@F2\` — dropped delivery healed by redelivery/reconcile | github / \`github.feature\` | \`GITHUB_TOKEN\` + \`GITHUB_ORG\` repo |
| \`@F3\` — repo created after boot appears within one reconcile | github / \`github.feature\` | \`GITHUB_TOKEN\` + \`GITHUB_ORG\` repo |
| \`@F7\` — API usage stays under budget at steady state | github / \`github.feature\` | \`GITHUB_TOKEN\` + \`GITHUB_ORG\` repo |
| \`@AC-GH-FORWARD\` — live \`gh webhook forward\` streams real deliveries | github / \`github.feature\` | \`GITHUB_TOKEN\` + \`gh\` CLI + public tunnel URL |
| \`@AC-GH-NOTIFY-304\` — \`/notifications\` honours If-Modified-Since + X-Poll-Interval | github / \`github.feature\` | \`GITHUB_TOKEN\` + \`GITHUB_ORG\` repo |
| \`@AC-GH-RATELOG\` — live rate-limit fields are logged | github / \`github.feature\` | \`GITHUB_TOKEN\` + \`GITHUB_ORG\` repo |
| \`@AC-GHQ-LIVE-WARM\` — \`issuesForRepo\` matches the live REST listing after a warm | github / \`github-query.feature\` | \`GITHUB_TOKEN\` + a repo under \`GITHUB_ORG\` |
| \`@S2\` — cross-box free-slot query warm + sub-second | peer / \`peer.feature\` | a real second box (non-loopback mesh) |
| \`@F8\` — a stopped box reads stale-since within 30 s (real-timing part (c)) | peer / \`peer.feature\` | a real second box (non-loopback mesh) |
| \`@AC-PEER-AUTH\` — a mesh peer rejects a wrong bearer with 401, mirrors nothing | peer / \`peer.feature\` | a real second box + per-peer bearer token |
| \`@AC-CLAUDE-PANE\` — a real Claude session in a tmux pane maps to a ClaudeInstance | claude / \`claude.feature\` | a real \`tmux\` + Claude Code install on a box |
| Telegram | — | n/a (out of spike scope) |

**Umbrella (plugin-tier, need the full fleet co-booted) — \`@pending\`, NOT \`@live\`.**
Not part of the \`@live\` count above: \`features/prd.feature\` \`@S1 @S2 @S4 @S5 @S6 @F1 @F4 @F5 @F6 @F8\`
and the cross-box \`S1\` (issue→PR ≤ 15 min), pending until every tier co-boots on real boxes.

## What the always-on worker needs from the graph
A dispatcher polling this graph today would compose these **real** ops/subscriptions (names
from \`plugins/*/schema/*.graphqls\` and \`plugins/github/queries/*.graphql\`). Gaps are listed as
follow-ups, not implemented:

- **Open, unassigned issues without a \`grinding\` label.** Read \`issuesForRepo(owner, repo)\`
  (\`github.graphqls:31\`; the cache warmed by the \`openIssues\` op, \`queries/openIssues.graphql\`,
  which fetches \`labels(first: 20)\` and \`states: OPEN\`), then filter client-side. **Gap:**
  \`issuesForRepo\` takes only owner+repo — no \`label\`/\`assignee\` filter arg — and the cached
  \`Issue\` exposes \`labels\` but **no assignee field** (\`openIssues.graphql\` never selects
  \`assignees\`), so "unassigned" is not expressible today. The \`myClaimed\` op
  (\`queries/myClaimed.graphql\`, \`filterBy: { assignee: \$login }\`) proves only the *claimed-by-me*
  direction. Follow-up: add an \`assignees\` selection to \`openIssues\` + an assignee/label filter arg.
- **Claim-visibility latency after a webhook.** There is **no** github assignment subscription —
  the only github stream is \`checkRunUpdated\` (\`github.graphqls:6\`). An assignment becomes visible
  only after the push/reconcile ingest lands it in the cache, then re-read via \`issuesForRepo\`/\`issue\`.
  Expected bound: one push-ingest / reconcile cycle (PRD F1), **not measured in this spike** (needs a
  live PAT — see Blocked \`@F2\`/\`@F3\`). The claude half (a worker announcing its claim) *is* live via
  \`claudeSessionUpdated(hostId)\` (\`claude.graphqls:17\`). Follow-up: an \`issueUpdated\`/assignment
  subscription so claim latency is push, not poll.
- **Check-run status for a PR.** \`subscription { checkRunUpdated }\` (\`github.graphqls:6\`, a
  \`GithubEvent\`) pushes CI transitions; \`checkRunsForPR(owner, repo, number)\`
  (\`queries/checkRunsForPR.graphql\`) reads them on demand. **Gap:** \`checkRunUpdated\` takes no
  PR argument — it fans every repo's check-run events, so the worker filters by PR client-side.
- **A free tmux pane to launch into.** \`freeSlots(hostId: String): [Slot!]!\` (\`tmux.graphqls:49\`);
  each \`Slot\` carries \`paneKey\` (\`tmux.graphqls:33\`). Live pane changes push on \`tmuxEvents\`
  (\`tmux.graphqls:54\`).
- **Live claude sessions.** \`claudeSessions(hostId, issueNumber)\` (\`claude.graphqls:8\`) for the
  current set (each \`ClaudeSession\` exposes \`state\`, \`gitBranch\`, \`issueNumber\`), with
  \`claudeSessionUpdated(hostId)\` (\`claude.graphqls:17\`) for the live push.

**Follow-up gaps (none implemented):** (1) no assignee field/filter for the unassigned-issue query;
(2) no assignment/\`issueUpdated\` subscription, so claim latency is poll-bound; (3) \`checkRunUpdated\`
is not PR-scoped; (4) cross-box variants of all of the above ride \`peers\` mirror rows, whose real
timing stays blocked (§ Blocked, \`@F8\`).
EOF

log "wrote $RESULTS"
echo "OK"
