# supergraph dev harness. `make dev` is the one-command bring-up (AC-CORE-15):
# it builds, points every plugin db at a throwaway .dev/data dir (never the real
# ~/.local/share), and runs the server in the foreground with the template plugin.
.PHONY: build generate test features features-red dev dev-check
include mk/version.mk

BIN := bin/supergraph
DEV_DIR := .dev
DEV_CONFIG := $(DEV_DIR)/config.toml
LISTEN := 127.0.0.1:7788

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/supergraph

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

# dev-config writes a throwaway config only if one is not already present, so a
# hand-edited .dev/config.toml is never clobbered.
$(DEV_CONFIG):
	mkdir -p $(DEV_DIR)
	printf 'hostId = "dev"\nlisten = "%s"\ndataDir = "%s/data"\ncanaryIntervalSeconds = 5\nlagThresholdSeconds = 30\n' "$(LISTEN)" "$(DEV_DIR)" > $(DEV_CONFIG)

dev: build $(DEV_CONFIG)
	$(BIN) --config $(DEV_CONFIG) serve

# dev-check is the non-interactive form used by AC-CORE-15: it backgrounds serve,
# polls /health until it returns 200 with a "template" entry (bounded 10s), prints
# that body, then always kills serve. Exits 0 on success, 1 on timeout.
dev-check: build $(DEV_CONFIG)
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
