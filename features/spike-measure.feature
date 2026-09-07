Feature: Spike measurement harness (no owner credentials)
  As the owner
  I want the post-tier §B spike (AC-GHQ-P95 latency + two-box peer stale/recovery) to be one
  reproducible command with committed numbers
  So that the two local-only claims are proven and the creds-blocked rest is enumerated honestly

  # @local checks the harness is present, wired into make, and that the committed results doc
  # carries the required sections + an explicit PASS/FAIL token per AC — WITHOUT the heavy run.
  # The full end-to-end run (build, real tmux, two serves, timing) is @slow: excluded from the
  # default suite, opt in with FEATURES_TAGS=@slow. See docs/edr/spike-measure.md.

  @local @spike @AC-SPIKE-SCRIPT
  Scenario: The spike-measure script exists and is executable
    Given the repo root
    Then the file `scripts/spike-measure.sh` exists and is executable

  @local @spike @AC-SPIKE-MAKE
  Scenario: make spike-measure resolves to the script
    When `make spike-measure -n` is run
    Then the resolved recipe names `scripts/spike-measure.sh`

  @local @spike @AC-SPIKE-DOC
  Scenario: The results doc carries the required sections and a PASS/FAIL token per AC
    Given the results doc `docs/spike-results.md`
    Then it has a section heading for the (a) latency AC and one for the (c) peer AC
    And it has an "## Environment" section and a "Blocked on owner credentials" section
    And it has a "What the always-on worker needs from the graph" section
    And each of the (a) and (c) sections carries a PASS or FAIL token

  @slow @spike @AC-SPIKE-RUN
  Scenario: The full spike run measures both ACs end to end, writes to a temp dir, leaves no residue
    When `scripts/spike-measure.sh` is run to completion
    Then the SUMMARY block reports a primary p95 verdict and a stale-marking time
    And the generated results doc has the required sections and a PASS/FAIL token per AC
    And the committed `docs/spike-results.md` is unchanged in git
    And no leftover spike process or tmux `-L sgmeasure` session remains
