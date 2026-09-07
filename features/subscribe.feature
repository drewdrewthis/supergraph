Feature: subscribe CLI — a graphql-transport-ws client for one plugin subscription field
  As the orchardist and CLI users
  I want `supergraph subscribe <field>` to print live events to stdout as they arrive
  So that a shell pipeline can react to issueUpdated/checkRunUpdated/claudeSessionUpdated/tmuxEvents
  without writing a bespoke websocket client

  # cmd/supergraph/subscribe.go dials core's /graphql endpoint over graphql-transport-ws
  # (server/server.go gqlgen transport.Websocket), sends connection_init/subscribe per
  # the protocol, and prints each pushed event's data object as one compact JSON line.
  # Unit coverage (query building, wsEndpoint, bearer-header gating, protocol framing,
  # reconnect/backoff) lives in cmd/supergraph/subscribe_test.go; these @local scenarios
  # only prove the CLI end to end against a real server subprocess.

  @subscribe @local @AC-SUB-1
  Scenario: An unknown field is a usage error, exit 1, nothing on stdout
    When I run `supergraph subscribe bogusField`
    Then exit code is 1
    And stdout is empty

  @subscribe @local @AC-SUB-4 @AC-SUB-5 @AC-SUB-7
  Scenario: subscribe --once prints one compact JSON line for a claudeSessionUpdated event
    Given a supergraph server started with the claude plugin and data dir <tmp>
    When I run `supergraph subscribe claudeSessionUpdated --once` against it in the background
    And a `SessionStart` hook payload for session `SUB1` is POSTed to `/plugins/claude/hook`
    Then the subscribe process exits 0 within 5s
    And its stdout is exactly one compact JSON line naming type `claude.session.updated`

  @subscribe @local @AC-SUB-6
  Scenario: subscribe against an endpoint that never accepts exhausts retries and exits 2
    When I run `supergraph subscribe tmuxEvents --once --endpoint http://127.0.0.1:1/graphql`
    Then exit code is 2
    And stderr is not empty

  # --- AC Coverage Map ---
  # AC-SUB-1 → unknown field: usage error, exit 1, nothing on stdout (unit: TestRunSubscribe_UnknownField*)
  # AC-SUB-2 → endpoint resolution + bearer only on non-loopback with a token (unit: TestSubscribeAuthHeader_*)
  # AC-SUB-3 → query building per field (unit: TestBuildSubscription_*)
  # AC-SUB-4 → one compact JSON line per next event (unit: TestRunSubscribe_OncePrintsOneCompactJSONLine*; here: claudeSessionUpdated scenario)
  # AC-SUB-5 → --once exits 0 after the first event (unit + here)
  # AC-SUB-6 → reconnect up to N times with backoff, exit 2 + stderr on exhaustion (unit: TestRunSubscribe_Reconnects*/Exhausts*; here: bogus endpoint)
  # AC-SUB-7 → graphql-transport-ws handshake: connection_init -> ack -> subscribe(id) -> next/error/complete (unit: TestRunSubscribe_SendsConnectionInitThenSubscribeWithID; here: real handshake against a live server)
