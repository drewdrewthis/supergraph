# supergraph dev harness. `make dev` is the one-command bring-up (AC-CORE-15):
# it builds, points every plugin db at a throwaway .dev/data dir (never the real
# ~/.local/share), and runs the server in the foreground with the template plugin.
.PHONY: build build-harness generate test features features-red features-pending features-github loc-github features-peer loc-peer dev dev-check
include mk/version.mk

BIN := bin/supergraph
DEV_DIR := .dev
DEV_CONFIG := $(DEV_DIR)/config.toml
LISTEN := 127.0.0.1:7788

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/supergraph

# build-harness is the dev build: it adds -tags harness so the harness-only sibling
# plugin (fakeok) is compiled in for the panic-isolation demo. Production `build`
# never sets the tag, so fakeok never ships.
build-harness:
	go build -tags harness -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/supergraph

generate:
	go run github.com/99designs/gqlgen generate

test:
	go test ./...

# features runs the godog .feature suite (green by default: @unmet excluded).
features:
	go test ./features/ -run TestFeatures -v

# features-red proves the runner fails red on an unmet scenario (AC-CORE-9): the
# @unmet-only run MUST exit non-zero, so `!` inverts it — this target succeeds
# only when the suite fails, and fails loudly if an unmet scenario ever passes.
features-red:
	! FEATURES_TAGS=@unmet go test ./features/ -run TestFeatures

# features-pending lists the PRD scenarios (S1-S6, F1-F8) not yet backed by a
# plugin tier; strict mode makes a non-empty pending set exit non-zero, so a
# non-zero exit here is the expected/reportable state, not a failure to fix.
features-pending:
	FEATURES_TAGS=@pending go test ./features/ -run TestFeatures

# features-github runs only the github plugin's @local scenarios (AC-GH-*),
# excluding both @pending and @live (a passing live scenario drops @pending but
# keeps @live, so ~@pending alone would run it without creds; ~@live keeps it out).
features-github:
	FEATURES_TAGS="@github && ~@pending && ~@live" go test ./features/ -run TestFeatures -v

# loc-github guards the plugins/github/** LOC budget (docs/edr/github.md): prod
# code only (no _test.go, no internal/fakegh), comments/blank lines stripped,
# fails when the total exceeds 1570 (AC-GH-LOC / AC-GHQ-LOC; cap raised
# 800→1300→1350→1540→1570 by owner — the 1350→1540 ratchet paid for the typed
# Query.issue/pullRequest/issuesForRepo reads + the cross-plugin join accessors
# (measured 1466); the 1540→1570 ratchet paid for the security-review fixes
# (nodesByKind LIKE-escaping, owner/repo validation, the per-request join memo, and
# the single.Ptr alignment), landing at measured 1486; 1486 x1.05 rounded up to 10 =
# 1570). Uses POSIX [[:space:]] (not \s,
# which BSD/macOS sed does not honor, silently under-stripping indented comments).
loc-github:
	@files=$$(find plugins/github -name '*.go' ! -name '*_test.go' -not -path '*/fakegh/*' 2>/dev/null); \
	if [ -z "$$files" ]; then count=0; else count=$$(echo "$$files" | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$$/d' | wc -l | tr -d ' '); fi; \
	echo "plugins/github prod LOC: $$count"; \
	if [ "$$count" -gt 1570 ]; then \
		echo "loc-github: $$count LOC exceeds the 1570 budget" >&2; \
		exit 1; \
	fi

# features-peer runs only the peer plugin's @local scenarios (AC-PEER-*), excluding
# both @pending and @live (opt-in via make features-live). The binary is built with
# -tags harness so the harness-only fakeremote source executor is compiled into the
# second process.
features-peer:
	FEATURES_TAGS="@peer && ~@pending && ~@live" go test ./features/ -run TestFeatures -v

# loc-peer guards the plugins/peer/** prod LOC budget (docs/edr/peer.md): prod code
# only (no _test.go), comments/blank lines stripped, fails when the total exceeds
# 680 (AC-PEER-LOC; 700->680 after the config helpers moved to plugins/internal/pluginconfig,
# measured 644 x1.05 rounded up to 10). Same portable formula as loc-github — POSIX [[:space:]] (not
# \s, which BSD/macOS sed does not honor). There is no internal/fake dir to exclude:
# the seeded remote lives in plugins/fakeremote (a separate, harness-only package).
loc-peer:
	@files=$$(find plugins/peer -name '*.go' ! -name '*_test.go' 2>/dev/null); \
	if [ -z "$$files" ]; then count=0; else count=$$(echo "$$files" | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$$/d' | wc -l | tr -d ' '); fi; \
	echo "plugins/peer prod LOC: $$count"; \
	if [ "$$count" -gt 680 ]; then \
		echo "loc-peer: $$count LOC exceeds the 680 budget" >&2; \
		exit 1; \
	fi

# dev-config writes a throwaway config only if one is not already present, so a
# hand-edited .dev/config.toml is never clobbered.
$(DEV_CONFIG):
	mkdir -p $(DEV_DIR)
	printf 'hostId = "dev"\nlisten = "%s"\ndataDir = "%s/data"\nlagThresholdSeconds = 30\n' "$(LISTEN)" "$(DEV_DIR)" > $(DEV_CONFIG)

dev: build-harness $(DEV_CONFIG)
	$(BIN) --config $(DEV_CONFIG) serve

# dev-check is the non-interactive form used by AC-CORE-15: it backgrounds serve,
# polls /health until it returns 200 with a "template" entry (bounded 10s), prints
# that body, then always kills serve. Exits 0 on success, 1 on timeout.
dev-check: build-harness $(DEV_CONFIG)
	@set -e; \
	$(BIN) --config $(DEV_CONFIG) serve & \
	SERVE_PID=$$!; \
	trap 'kill $$SERVE_PID 2>/dev/null || true' EXIT; \
	ok=0; \
	for i in $$(seq 1 50); do \
		body=$$(curl -fsS http://$(LISTEN)/health 2>/dev/null || true); \
		if printf '%s' "$$body" | grep -q '"plugin":"template"'; then \
			echo "$$body"; ok=1; break; \
		fi; \
		sleep 0.2; \
	done; \
	if [ "$$ok" != "1" ]; then echo "dev-check: template not healthy within 10s" >&2; exit 1; fi

# --- claude plugin targets ---
.PHONY: features-claude loc-claude

# features-claude runs only the claude plugin's @local scenarios (AC-CLAUDE-*, F1/F5/S4
# claude halves), excluding both @pending and @live (opt-in via make features-live).
features-claude:
	FEATURES_TAGS="@claude && ~@pending && ~@live" go test ./features/ -run TestFeatures -v

# loc-claude guards the plugins/claude/** LOC budget (docs/edr/claude.md): prod code
# only (no _test.go), comments/blank lines stripped, fails when the total exceeds 750
# (AC-CLAUDE-LOC; 790->750 after the config helpers moved to plugins/internal/pluginconfig,
# measured 710 x1.05 rounded up to 10). Uses POSIX [[:space:]] (not \s, which BSD/macOS sed does not honor,
# silently under-stripping indented comments) — the same formula as loc-github.
loc-claude:
	@files=$$(find plugins/claude -name '*.go' ! -name '*_test.go' 2>/dev/null); \
	if [ -z "$$files" ]; then count=0; else count=$$(echo "$$files" | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$$/d' | wc -l | tr -d ' '); fi; \
	echo "plugins/claude prod LOC: $$count"; \
	if [ "$$count" -gt 750 ]; then \
		echo "loc-claude: $$count LOC exceeds the 750 budget" >&2; \
		exit 1; \
	fi

# --- subscribe CLI targets ---
.PHONY: features-subscribe

# features-subscribe runs only the subscribe CLI's @local scenarios (AC-SUB-*),
# excluding both @pending and @live (opt-in via make features-live).
features-subscribe:
	FEATURES_TAGS="@subscribe && ~@pending && ~@live" go test ./features/ -run TestFeatures -v

# --- tmux plugin targets (appended; see docs/edr/tmux.md) ---
.PHONY: features-tmux loc-tmux loc-internal

# TMUX_LOC_CAP is the ratified LOC budget for the tmux plugin: 600 -> 800 (control
# client + poll + read model) -> 860 after the B1/B2 bug fixes and review Shoulds
# landed at a measured 813, then 860->820 after the config helpers moved to
# plugins/internal/pluginconfig (measured 776 x1.05 rounded up to 10). Owner rule:
# measured + 5% rounded up to a multiple of 10. See the LOC table in docs/edr/tmux.md.
TMUX_LOC_CAP := 820

# features-tmux runs only the tmux plugin's @local scenarios against a real tmux
# server on a private -L socket (created and torn down per scenario). Requires the
# tmux binary on PATH; CI installs it (see .github/workflows/ci.yml).
features-tmux:
	FEATURES_TAGS="@tmux && @local" go test ./features/ -run TestFeatures -v

# loc-tmux prints the tmux plugin's non-comment non-blank non-test LOC and fails if
# it exceeds TMUX_LOC_CAP. The sed program strips whole-line `//` comments and blank
# lines; it is line-oriented and portable (BSD/GNU sed both accept -E with it),
# matching the count reported in the EDR LOC table.
loc-tmux:
	@n=$$(find plugins/tmux -name '*.go' ! -name '*_test.go' -print0 \
		| xargs -0 sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$$/d' \
		| grep -c '.'); \
	echo "tmux plugin LOC: $$n (cap $(TMUX_LOC_CAP))"; \
	if [ "$$n" -gt "$(TMUX_LOC_CAP)" ]; then \
		echo "loc-tmux: $$n exceeds cap $(TMUX_LOC_CAP)" >&2; exit 1; \
	fi

# --- shared internal helper targets ---
# loc-internal guards the plugins/internal/** shared-helper LOC budget: prod code
# only (no _test.go), comments/blank lines stripped. Cap 100 = measured 89 x1.05
# rounded up to a multiple of 10 (owner rule); holds at 100 after plugins/internal/single
# (single.Ptr[T], the peer/claude/tmux singleton seam) landed, measured 89->90. Holds the
# extracted config-coercion helpers (plugins/internal/pluginconfig) the 4 plugins now
# share, plus single; a single umbrella budget so future plugins/internal/* helpers
# share one gate.
loc-internal:
	@files=$$(find plugins/internal -name '*.go' ! -name '*_test.go' 2>/dev/null); \
	if [ -z "$$files" ]; then count=0; else count=$$(echo "$$files" | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$$/d' | wc -l | tr -d ' '); fi; \
	echo "plugins/internal prod LOC: $$count"; \
	if [ "$$count" -gt 100 ]; then \
		echo "loc-internal: $$count LOC exceeds the 100 budget" >&2; \
		exit 1; \
	fi

# --- shared branch↔issue derivation gate (appended; docs/edr/github-query.md) ---
.PHONY: loc-issuekey

# loc-issuekey guards the shared internal/issuekey package's prod LOC (the
# single source of truth for the branch↔issue join key across claude/tmux/github).
# Cap 30 = measured 28 + 5% rounded up to 10. Same portable POSIX [[:space:]]
# formula as loc-github.
loc-issuekey:
	@files=$$(find internal/issuekey -name '*.go' ! -name '*_test.go' 2>/dev/null); \
	if [ -z "$$files" ]; then count=0; else count=$$(echo "$$files" | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$$/d' | wc -l | tr -d ' '); fi; \
	echo "internal/issuekey prod LOC: $$count"; \
	if [ "$$count" -gt 30 ]; then \
		echo "loc-issuekey: $$count LOC exceeds the 30 budget" >&2; \
		exit 1; \
	fi

# --- live scenarios (appended; README > Live scenarios) ---
.PHONY: features-live

# features-live runs the @live scenarios that are actually implemented (not the
# honest-@pending ones still blocked on infrastructure). It needs GITHUB_TOKEN +
# LIVE_REPO in the environment (see README). FEATURES_TAGS overrides the default
# tag expression, so ~@pending must be spelled out here to skip the blocked live
# scenarios; @live keeps them out of the hermetic `make features` run.
features-live:
	FEATURES_TAGS='@live && ~@pending' go test ./features/ -run TestFeatures -count=1 -v

# --- spike measurements (no owner creds; docs/plans/post-tier.md §B) ---
.PHONY: spike-measure spike-measure-publish

# spike-measure runs the no-creds spike battery: the AC-GHQ-P95 cross-plugin query
# p95/p99 over both the HTTP and CLI paths (part a) and the two-box peer stale-marking +
# recovery timing (part c). It builds a throwaway binary, drives a real `tmux -L
# sgmeasure` socket + two loopback `supergraph serve` peers, and prints a machine-
# readable SUMMARY. It writes the results doc to a throwaway temp dir (no repo churn);
# use spike-measure-publish to overwrite the committed docs/spike-results.md. See
# docs/edr/spike-measure.md.
spike-measure:
	@scripts/spike-measure.sh

# spike-measure-publish runs the same battery but points OUT at the committed results
# doc so it is regenerated in place. This is the only target that writes into the repo.
spike-measure-publish:
	@OUT=docs/spike-results.md scripts/spike-measure.sh
