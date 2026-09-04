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
