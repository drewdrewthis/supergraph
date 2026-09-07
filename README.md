# supergraph

One binary per box. It aggregates each data source (a plugin) into a single
GraphQL + health endpoint on `127.0.0.1:7788`.

## Install Go

Install Go 1.26 or newer from https://go.dev/dl/. Confirm:

```
go version
```

## Build

```
make build
```

The binary lands at `bin/supergraph`.

## Run locally (dev harness)

```
make dev
```

This builds, writes a throwaway `.dev/config.toml`, and runs the server in the
foreground with the template plugin.

## Query the server

In a second terminal:

```
supergraph query '{ health { plugin state } }'
```

Run a named operation from a plugin's `queries/` dir with `--op` (and `--plugin` to
pick the plugin, default `github`):

```
# github ops post to the github plugin's op route (cached, node-keyed):
supergraph query --op openPRs --plugin github

# claude ops post their query text to core /graphql (a plain read):
supergraph query --op sessionsForIssue --plugin claude --var issueNumber=1
supergraph query --op instances --plugin claude
```

`--op NAME` loads `./plugins/<plugin>/queries/NAME.graphql` (override the dir with
`--queries-dir`). Only plugins that expose an op route (currently `github`) key the
returned nodes; every other plugin's op is run as a normal query against core.

Read health over HTTP:

```
curl http://127.0.0.1:7788/health
```

Print the build version:

```
supergraph --version
```

## Test

```
make test
```

Run the BDD feature suite (green by default):

```
make features
```

Prove the runner fails red on an unmet scenario:

```
make features-red
```

## Live scenarios

The `@live` scenarios exercise the plugins against **real** external services and
are excluded from the default `make features` run (hermetic). Run the implemented
ones with:

```
make features-live
```

which is `FEATURES_TAGS='@live && ~@pending' go test ./features/ -run TestFeatures -v`.
`FEATURES_TAGS='@live'` (without `~@pending`) additionally runs the live scenarios
that are still honest `@pending` (blocked on infrastructure — see below); those
report pending, not pass.

**Environment variables**

| Var | Meaning |
| --- | --- |
| `GITHUB_TOKEN` | A GitHub PAT. Passed to the serve subprocess via the inherited env only — **never** written to `config.toml` or any repo file. `export GITHUB_TOKEN="$(gh auth token)"`. |
| `LIVE_REPO` | `owner/repo` the github live scenarios target (e.g. `drewdrewthis/supergraph`). A bare `repo` is also accepted when `GITHUB_ORG` is set. |
| `GITHUB_ORG` | Optional owner used when `LIVE_REPO` is a bare repo name. |

**`[plugins.github]` ingest config** (config.toml, not env): `forwardRepo` (`owner/repo`) or
`forwardOrg` (name) sets the `gh webhook forward` target — one is **required** on `forward` ingress
(neither ⇒ the forward child does not start; reconcile still heals). `forwardEvents` overrides the
default `--events` list (the exact events the plugin ingests). `hookRepos` is the `owner/repo`
allowlist for webhook creation — **default empty ⇒ the plugin creates no hooks**; list a repo to opt
it in. Hooks the plugin creates are logged with their `owner/repo`, id, and callback URL so they are
identifiable; **the plugin does not delete them on shutdown** — remove them manually (below).

```
export GITHUB_TOKEN="$(gh auth token)"
export LIVE_REPO="drewdrewthis/supergraph"
make features-live
```

**Prerequisites**

- `gh` logged in with a token carrying at least the `repo` scope (`gh auth status`).
- The `cli/gh-webhook` extension (`gh extension install cli/gh-webhook`) for the
  webhook-forward ingress path.
- A real Claude Code install and `tmux` for the `@claude` live pane-mapping proof.
- A second real mesh box for the `@peer` cross-box proofs.

**What runs green today** (`make features-live`)

- `@AC-GHQ-LIVE-WARM` — `issuesForRepo` matches the live REST open-issue listing
  after a warm.
- `@AC-GH-RATELOG` — a live GraphQL request logs `rateLimit{remaining,resetAt}`.
- `@AC-GH-NOTIFY-304` — the `notifications` poll conditionally GETs `/notifications`
  and an unchanged poll returns 304 for zero quota (waits across two ~60s polls).

**Teardown / verification**

The green scenarios are **read-only**: the plugin runs with a 3600s reconcile
interval that never fires inside a scenario window, so **no webhooks are created**
and nothing is mutated on the account. `hookRepos` also defaults to empty, so even a
reconcile that fired would create no hooks. Any webhook the plugin creates is a `web`
hook identifiable by its callback URL path (`…/plugins/github/webhook`) and a
`created "web" webhook …` log line; verify and clean up with:

```
gh api repos/$LIVE_REPO/hooks            # list; expect none from a features-live run
```

Every serve subprocess and `gh webhook forward` child is terminated at scenario
teardown; confirm none leaked:

```
pgrep -fl 'supergraph serve|gh webhook'  # expect empty after a run
```

**Tokens must never appear in logs.** The PAT is passed only through the process
environment. Do not `echo`/`set -x`/log it, and redact any captured output
(`sed -E 's/gh[po]_[A-Za-z0-9]+/REDACTED/g'`) before sharing.

## Install as a service

```
supergraph install
supergraph start
supergraph status
supergraph stop
supergraph uninstall
```

`install` writes a `systemd --user` unit on Linux or a `launchd` plist on macOS.
It is idempotent. After replacing the binary, run `supergraph restart`.

Read logs:

```
journalctl --user -u supergraph          # Linux
log show --predicate 'process == "supergraph"'   # macOS
```

## Docs

- PRD: https://github.com/drewdrewthis/supergraph/blob/main/docs/PRD.md
- Plugin contract: https://github.com/drewdrewthis/supergraph/blob/main/docs/plugin-contract.md
- Core-tier plan: https://github.com/drewdrewthis/supergraph/blob/main/docs/plans/core-tier.md
- Tracking issue: https://github.com/drewdrewthis/supergraph/issues/1
