Feature: justfile agent tooling layer over `supergraph query --op`
  As an agent driving a live `supergraph serve`
  I want a documented `just` recipe tree, anchored per-plugin queries-dir, and a
  drift gate over their op files
  So that I can call cross-plugin recipes from any cwd without hand-building
  `supergraph query` invocations or silently masking a failed query as null

  # `make` stays build/test/CI; `just` (root justfile + one `mod.just` per plugin,
  # next to that plugin's `queries/` dir) is the runtime surface agents call
  # against a live server. Every scenario here is @local: no live server, no
  # network, no credentials — `just --list`, `scripts/just-check.sh`, and `just`
  # itself are exercised directly against the filesystem, with `SUPERGRAPH_BIN`
  # pointed at a fake binary (never the real `supergraph`) wherever a recipe
  # needs something to shell out to.
  #
  # OWNERSHIP: this feature owns the justfile tooling layer's HERMETIC surface
  # only. The live-server ACs — AC-JUST-QUERY-NESTED, AC-JUST-QUERY-DIRECT,
  # AC-JUST-MUTATE, and the installer/global-install ACs (scripts/install-remote.sh
  # + `just -g`) — need a real `supergraph serve` and/or a GitHub token and are
  # covered by captured use-proof on the PR, not by a scenario in this suite.
  #
  # `just` must be on PATH for any scenario here to run; if it is absent the
  # scenario skips cleanly (godog ErrSkip) rather than failing, since CI's `test`
  # job does not install `just`.

  @justfile @local @AC-JUST-LIST
  Scenario: Every recipe just --list prints carries a doc comment
    Given the repo's root justfile and its plugin modules
    When `just --list` is run at the repo root, and `just --list <module>` for each plugin module
    Then every recipe row in every listing has a non-empty doc comment after its `#`

  @justfile @local @AC-JUST-OPS
  Scenario: The op-file drift gate passes on the shipped tree
    Given the repo's shipped plugins and their mod.just files
    When `scripts/just-check.sh` is run against the repo root
    Then it exits 0

  @justfile @local @AC-JUST-DRIFT
  Scenario: The op-file drift gate fails when a query file goes missing
    Given a temp copy of the repo's plugins and scripts/just-check.sh
    When one plugin's `.graphql` op file is deleted from the copy
    And `scripts/just-check.sh` is run against the copy
    Then it exits non-zero
    And its output names the missing plugin and op

  @justfile @local @AC-JUST-EXIT
  Scenario: A failing query binary exits the recipe non-zero with no masked success
    Given `SUPERGRAPH_BIN` pointed at a fake binary that prints an error to stderr and exits 1
    When a query recipe is run against that binary
    Then the recipe exits non-zero
    And stdout is empty and does not contain "null"

  @justfile @local @AC-JUST-CWD
  Scenario: A recipe anchors its queries-dir from a foreign working directory
    Given `SUPERGRAPH_BIN` pointed at a fake binary that echoes its own argv as JSON
    And the current working directory is a temp dir outside the repo
    When a tmux plugin recipe is run with `just --justfile` pointed at the repo's justfile
    Then the captured argv's `--queries-dir` is an absolute path under the repo's `plugins/tmux/queries`
    And the file it names exists

  @justfile @local @AC-JUST-COLLIDE
  Scenario: Same-named op files across plugins resolve to their own plugin's directory
    Given `SUPERGRAPH_BIN` pointed at a fake binary that echoes its own argv as JSON
    When `just github branch-ref` is run
    And `just tmux pane-for-branch` is run
    Then each recipe's captured `--queries-dir` resolves under its own plugin's `queries` directory
    And both plugins' `paneForBranch.graphql` files exist and are different files

  # --- AC Coverage Map ---
  # AC-JUST-LIST:    "just --list, every recipe documented" -> Scenario: Every recipe just --list prints carries a doc comment
  # AC-JUST-OPS:     "just-check.sh green on shipped tree" -> Scenario: The op-file drift gate passes on the shipped tree
  # AC-JUST-DRIFT:   "just-check.sh red on a missing op file" -> Scenario: The op-file drift gate fails when a query file goes missing
  # AC-JUST-EXIT:    "failed query binary never masked as success (the | jq trap)" -> Scenario: A failing query binary exits the recipe non-zero with no masked success
  # AC-JUST-CWD:     "recipes resolve op files from a foreign cwd" -> Scenario: A recipe anchors its queries-dir from a foreign working directory
  # AC-JUST-COLLIDE: "same-named op files across plugins don't collide" -> Scenario: Same-named op files across plugins resolve to their own plugin's directory
  # AC-JUST-QUERY-NESTED, AC-JUST-QUERY-DIRECT, AC-JUST-MUTATE, installer/global-install ACs:
  #   need a live `supergraph serve` and/or GitHub credentials -> covered by captured use-proof on the PR, not a scenario here.
