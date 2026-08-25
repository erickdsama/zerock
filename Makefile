# zerock - build, test and install
#
# Go 1.22+ is enough to bootstrap: the go directive pulls the toolchain this
# module needs. If go is not on PATH, try /usr/local/go/bin/go.

GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Stamped into every build so a deployed binary can be identified. Without it,
# two builds both call themselves "dev" and there is no way to tell whether the
# one on the server is the one you just built.
BUILD   ?= $(shell date -u +%Y%m%dT%H%M%SZ)
LDFLAGS := -s -w \
	-X github.com/erick/zerock/internal/version.Version=$(VERSION) \
	-X github.com/erick/zerock/internal/version.Build=$(BUILD)
PREFIX  ?= /usr/local

.PHONY: all build build-cli test race vet fmt check boundary install clean release

all: check build

# The full binary: client verbs plus the server. This is what runs on the host
# that owns the domain.
build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/zerock ./cmd/zerock

# The client build: the same CLI minus the server verbs, and so minus certmagic,
# bbolt, the DNS providers and zap. Roughly half the size, and it is the one
# most people install.
build-cli:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/zerock-cli ./cmd/zerockcli

test:
	$(GO) test ./...

# The server multiplexes concurrent streams, so the race detector is the test
# run that matters most.
race:
	$(GO) test -race -count=2 ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# The client build must not be able to reach the server. Without this guard a
# stray import silently doubles the client binary and drags ACME and the
# database back into it, which is exactly the state this split undid.
boundary:
	@if $(GO) list -deps ./cmd/zerockcli | grep -qE 'zerock/internal/(server|store)'; then \
		echo "error: the client build reaches internal/server or internal/store:"; \
		$(GO) list -deps ./cmd/zerockcli | grep -E 'zerock/internal/(server|store)' | sed 's/^/  /'; \
		exit 1; \
	fi
	@echo "✓ client build carries no server code"

check: vet boundary test

install: build
	install -Dm755 bin/zerock $(DESTDIR)$(PREFIX)/bin/zerock

# Static binaries for the common targets. Two artifacts per platform:
# zerock-<os>-<arch>        the client, what people install
# zerock-server-<os>-<arch> the full binary, what the host runs
# The server is only useful on Linux, so it is not built for darwin.
release:
	@mkdir -p dist
	@rm -f dist/SHA256SUMS
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "building dist/zerock-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/zerock-$$os-$$arch ./cmd/zerockcli || exit 1; \
	done
	@for target in linux/amd64 linux/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "building dist/zerock-server-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/zerock-server-$$os-$$arch ./cmd/zerock || exit 1; \
	done
	@cd dist && sha256sum zerock-* > SHA256SUMS
	@echo "✓ dist/ built, checksums in dist/SHA256SUMS"

clean:
	rm -rf bin dist
