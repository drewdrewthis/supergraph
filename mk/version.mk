# version.mk — build-time version stamping for cmd/supergraph.
#
# VERSION is the git-describe stamp (tag + commits-since + short-sha + -dirty),
# falling back to the short sha, then "dev". LDFLAGS injects it into main.version
# so `supergraph --version` and the serve boot log report the exact build.
#
# To wire this into the Makefile, add near the top:
#     include mk/version.mk
# and change the build recipe to:
#     go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/supergraph
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
