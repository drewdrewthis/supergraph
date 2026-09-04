Feature: Template plugin contract
  As a plugin author
  I want the template plugin to prove the zero-core-edit contract
  So that future plugins (tmux, claude, github, peer, telegram) can follow the same pattern

  @integration @AC-CORE-10
  Scenario: Adding the template plugin touches zero files under core/
    Given the template plugin exists under `plugins/template/`, registered via `graph/plugins_import.go` and regenerated `graph/`
    When I check that no file under core/ references the template plugin
    Then no core file imports a plugin package
    And a supergraph server started with the template plugin and data dir <tmp> serves a "template" entry in `/health`

  @integration @AC-CORE-10b
  Scenario: The extend-type seam resolves plugin-contributed query and subscription fields
    Given a supergraph server started with the template plugin and data dir <tmp>
    When I run `supergraph query '{ templatePing }'`
    Then stdout contains resolver data for "templatePing" that is not null
    And exit code is 0
    When I open a websocket subscription to `templateEvents` on `127.0.0.1:7788/graphql`
    And the template plugin emits a matching event
    Then at least one message is received on the `templateEvents` subscription

  @integration @AC-CORE-4
  Scenario: A panic in one plugin's Start does not take down the server or other plugins
    Given a supergraph server started with the template plugin, a second "fakeok" plugin that never panics, and data dir <tmp>
    When I induce a panic in the template plugin's Start via its panic-inject path
    And I wait for the panic to be detected
    Then the "template" entry in `/health` has state "stale"
    And `GET /health` still returns HTTP 200
    And the "fakeok" entry in `/health` has state "ok"
    And `supergraph query '{ __typename }'` still returns HTTP 200

  @unit @AC-CORE-12
  Scenario: A plugin self-registers via init() and duplicate names panic loudly
    Given the template plugin calls `core.Register` from its own `init()`
    When a supergraph server boots with the template plugin and data dir <tmp>
    Then the boot log lists "template" among the registered factories
    And no file under `core/` was edited to achieve this
    When a second plugin registers the name "template" a second time
    Then the registration panics with a message naming "template"

  @integration @AC-CORE-13
  Scenario: Migrate runs before Start at boot and is idempotent across restarts
    Given an empty data dir <tmp> with no template database
    When I run `supergraph serve` with the template plugin against data dir <tmp>
    Then `sqlite3 <tmp>/template.db .tables` lists the template plugin's tables
    When I run `supergraph stop`
    And I run `supergraph serve` again with the template plugin against data dir <tmp>
    Then exit code is 0
    And `sqlite3 <tmp>/template.db .tables` lists the same tables with no duplicates

  # --- AC Coverage Map ---
  # AC-CORE-4: "Panic isolation — F5" → Scenario: A panic in one plugin's Start does not take down the server or other plugins
  # AC-CORE-10: "S5 — zero core edit" → Scenario: Adding the template plugin touches zero files under core/
  # AC-CORE-10b: "Extend seam resolves" → Scenario: The extend-type seam resolves plugin-contributed query and subscription fields
  # AC-CORE-12: "Registry contract" → Scenario: A plugin self-registers via init() and duplicate names panic loudly
  # AC-CORE-13: "Migrate at boot, idempotent" → Scenario: Migrate runs before Start at boot and is idempotent across restarts
