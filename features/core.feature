Feature: Core-tier supergraph server
  As an operator of a supergraph box
  I want the core server, CLI, and plugin registry to behave correctly
  So that plugins can be added later without touching core code

  @integration @AC-CORE-1
  Scenario: Envelope round-trips byte-identical through the running server
    Given a supergraph server started with the template plugin and data dir <tmp>
    When the template plugin emits a "V:2" event captured from the templateEvents subscription
    Then the stored Payload is byte-identical to the emitted Payload
    And the emitted event's "v" field equals 2
    And exit code is 0

  @integration @AC-CORE-2
  Scenario: SQLite ring prunes to capacity and cursor survives reopen
    Given a template ring store with capacity 5 in data dir <tmp>
    When 10 events are appended through the ring
    Then a query of the ring returns exactly 5 rows
    When the ring store is closed and reopened
    Then a cursor written before the reopen is unchanged after it

  @integration @AC-CORE-3
  Scenario: Health endpoint and health query return matching shape
    Given a supergraph server started with the template plugin and data dir <tmp>
    When I run `curl -s 127.0.0.1:7788/health`
    Then exit code is 0
    And the JSON response is an array of objects with exactly the keys "plugin", "lastEventAt", "cursor", "lagSeconds", "state"
    And each object's "state" is one of "starting", "ok", "stale"
    And every object's "lastEventAt" is JSON null or a valid timestamp (never empty string or epoch)
    When I run `supergraph query '{ health { plugin state } }'`
    Then the GraphQL response matches the same plugin/state set as the `/health` response

  @integration @AC-CORE-6
  Scenario: CLI serve binds the port and query round-trips
    Given a supergraph server started with the template plugin and data dir <tmp>
    When I run `supergraph query '{ __typename }'`
    Then stdout contains "Query"
    And exit code is 0
    When I run `supergraph serve` a second time on the same port
    Then stderr contains an "address in use" error
    And exit code is non-zero

  @integration @service @linux @AC-CORE-7
  Scenario: Install is idempotent on Linux
    Given a clean data dir <tmp> with no supergraph service installed
    When I run `supergraph install`
    Then exit code is 0
    And `systemctl --user status supergraph` reports exactly one unit
    When I run `supergraph install` a second time
    Then exit code is 0
    And `systemctl --user status supergraph` still reports exactly one unit
    When I run `supergraph uninstall`
    Then `systemctl --user status supergraph` reports no unit

  @integration @service @darwin @AC-CORE-7
  Scenario: Install is idempotent on macOS
    Given a clean data dir <tmp> with no supergraph service installed
    When I run `supergraph install`
    Then exit code is 0
    And `launchctl list | grep supergraph` reports exactly one plist
    When I run `supergraph install` a second time
    Then exit code is 0
    And `launchctl list | grep supergraph` still reports exactly one plist
    When I run `supergraph uninstall`
    Then `launchctl list | grep supergraph` reports no plist

  @integration @AC-CORE-8
  Scenario: Local websocket subscription receives a pushed event
    Given a supergraph server started with the template plugin and data dir <tmp>
    When I open a websocket subscription to `templateEvents` on `127.0.0.1:7788/graphql`
    And the template plugin emits a matching event
    Then a pushed message is received on the subscription within 1s
    When I close the websocket subscription
    Then no error is logged by the server after the socket closes

  @integration @AC-CORE-9
  Scenario: The .feature runner reports a fully-satisfied scenario as green
    Given a supergraph server started with the template plugin and data dir <tmp>
    When I run `go test ./features/...` excluding scenarios tagged @unmet
    Then exit code is 0

  @integration @unmet @AC-CORE-9
  Scenario: A deliberately-unmet scenario fails the runner red
    Given a supergraph server started with the template plugin and data dir <tmp>
    Then this step is intentionally unmet

  @integration @AC-CORE-11
  Scenario Outline: Config validation gates startup on a present hostId
    Given a `config.toml` with <hostId state>
    When I run `supergraph serve` with that config
    Then exit code is <exit code>
    And <outcome>

    Examples:
      | hostId state              | exit code | outcome                                                |
      | a valid "hostId"          | 0         | the loaded config exposes "hostId", "peers", "tokens"  |
      | no "hostId" field         | non-zero  | stderr contains an error message naming "hostId"       |

  @integration @service @linux @darwin @AC-CORE-14
  Scenario: CLI lifecycle drives the installed service through start, status, and stop
    Given a supergraph service installed via `supergraph install`
    When I run `supergraph start`
    And I run `supergraph status`
    Then stdout reports the service running with a pid and port 7788
    And exit code is 0
    When I run `supergraph stop`
    And I run `supergraph status`
    Then stdout reports the service not running
    And a check of port 7788 shows it is free
    When I replace the installed binary with a new build
    And I run `supergraph restart`
    Then `supergraph query '{ __typename }'` reflects the new build's version marker

  @integration @AC-CORE-15
  Scenario: make dev brings up the server with the template plugin in one command
    Given a clean checkout with no server running
    When I run `make dev`
    Then within a bounded wait, `GET /health` returns HTTP 200
    And the JSON response includes a "template" entry
    And no file under the real `~/.local/share` data dir was written

  @unit @AC-CORE-16
  Scenario: The plugin contract doc matches the Plugin interface in code
    Given the files `core/plugin.go` and `docs/plugin-contract.md`
    When I diff the sed-extracted `type Plugin interface { ... }` block from each file
    Then the diff output is empty
    And exit code is 0

  @integration @AC-CORE-17
  Scenario: Concurrent reads succeed while a writer appends events
    Given a supergraph server started with the template plugin and data dir <tmp>
    When a writer loop appends template events continuously
    And a concurrent GraphQL read queries the "template" ring while the writer runs
    Then the concurrent read completes and returns rows
    And a grep of the run log for "database is locked" returns nothing

  # --- AC Coverage Map ---
  # AC-CORE-1: "Envelope contract, use-proof" → Scenario: Envelope round-trips byte-identical through query
  # AC-CORE-2: "SQLite ring + cursor, use-proof" → Scenario: SQLite ring prunes to capacity and cursor survives restart
  # AC-CORE-3: "Health endpoint shape" → Scenario: Health endpoint and health query return matching shape
  # AC-CORE-4: "Panic isolation — F5" → covered in plugins/template/template.feature
  # AC-CORE-5: "Canary positive-fire, named channel" → covered in plugins/template/template.feature
  # AC-CORE-6: "CLI serve + query round-trip" → Scenario: CLI serve binds the port and query round-trips
  # AC-CORE-7: "Install idempotent, OS-detected" → Scenario: Install is idempotent on Linux / Install is idempotent on macOS
  # AC-CORE-8: "Local subscription push" → Scenario: Local websocket subscription receives a pushed event
  # AC-CORE-9: ".feature runner red/green" → Scenario: The .feature runner reports a fully-satisfied scenario as green (+ deliberately-unmet counterpart)
  # AC-CORE-10: "S5 — zero core edit" → covered in plugins/template/template.feature
  # AC-CORE-10b: "Extend seam resolves" → covered in plugins/template/template.feature
  # AC-CORE-11: "Config validation" → Scenario: Config with hostId loads.../Config missing hostId fails startup with a named error
  # AC-CORE-12: "Registry contract" → covered in plugins/template/template.feature
  # AC-CORE-13: "Migrate at boot, idempotent" → covered in plugins/template/template.feature
  # AC-CORE-14: "CLI lifecycle" → Scenario: CLI lifecycle drives the installed service through start, status, and stop
  # AC-CORE-15: "`make dev` one command" → Scenario: make dev brings up the server with the template plugin in one command
  # AC-CORE-16: "Contract doc matches code" → Scenario: The plugin contract doc matches the Plugin interface in code
  # AC-CORE-17: "Concurrent read during write" → Scenario: Concurrent reads succeed while a writer appends events
