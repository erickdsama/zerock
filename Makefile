# zerock - build, test and install
#
# Go 1.22+ is enough to bootstrap: the go directive pulls the toolchain this
# module needs. When go is not on PATH, the standard install location is tried
# before giving up, so a fresh shell without the PATH tweak still builds.

GO      ?= $(shell command -v go 2>/dev/null || ([ -x /usr/local/go/bin/go ] && echo /usr/local/go/bin/go) || echo go)
MODULE  := $(shell sed -n 's/^module //p' go.mod)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Stamped into every build so a deployed binary can be identified. Without it,
# two builds both call themselves "dev" and there is no way to tell whether the
# one on the server is the one you just built.
BUILD   ?= $(shell date -u +%Y%m%dT%H%M%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Build=$(BUILD)
PREFIX  ?= /usr/local

.PHONY: all build build-cli build-tray test race vet fmt check boundary install clean release release-tray-darwin

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

# The tray widget: a menu bar / system tray icon that starts, watches and stops
# tunnels. Pure Go over D-Bus on Linux; on macOS it needs cgo for Cocoa, so it
# is built on a Mac (or by the release workflow's macOS job), never cross-built.
build-tray:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/zerock-tray ./cmd/zerocktray

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
	@if $(GO) list -deps ./cmd/zerocktray | grep -qE 'zerock/internal/(server|store)'; then \
		echo "error: the tray build reaches internal/server or internal/store:"; \
		$(GO) list -deps ./cmd/zerocktray | grep -E 'zerock/internal/(server|store)' | sed 's/^/  /'; \
		exit 1; \
	fi
	@echo "✓ client and tray builds carry no server code"

check: vet boundary test

install: build
	install -Dm755 bin/zerock $(DESTDIR)$(PREFIX)/bin/zerock

# Static binaries for every platform. Three artifacts per platform:
# zerock-<os>-<arch>        the client, what people install
# zerock-server-<os>-<arch> the full binary, what the host runs
# zerock-tray-<os>-<arch>   the tray widget
# Windows binaries carry .exe, and the Windows tray is linked as a GUI program
# so it opens no console. Everything cross-compiles with cgo off except the
# macOS tray, which needs Cocoa: see release-tray-darwin.
PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

release:
	@mkdir -p dist
	@rm -f dist/SHA256SUMS
	@for target in $(PLATFORMS); do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; trayflags=""; \
		if [ "$$os" = windows ]; then ext=.exe; trayflags="-H windowsgui"; fi; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/zerock-$$os-$$arch$$ext ./cmd/zerockcli || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/zerock-server-$$os-$$arch$$ext ./cmd/zerock || exit 1; \
		if [ "$$os" != darwin ]; then \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				$(GO) build -trimpath -ldflags "$(LDFLAGS) $$trayflags" -o dist/zerock-tray-$$os-$$arch$$ext ./cmd/zerocktray || exit 1; \
		fi; \
	done
	@cd dist && sha256sum zerock-* > SHA256SUMS
	@echo "✓ dist/ built, checksums in dist/SHA256SUMS"

# The macOS tray, both architectures. Runs on a Mac: Cocoa needs cgo, and Go's
# cgo picks the right -arch for the other one, so an arm64 Mac builds both.
release-tray-darwin:
	@mkdir -p dist
	@for arch in arm64 amd64; do \
		echo "building dist/zerock-tray-darwin-$$arch"; \
		CGO_ENABLED=1 GOOS=darwin GOARCH=$$arch \
			$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/zerock-tray-darwin-$$arch ./cmd/zerocktray || exit 1; \
	done

clean:
	rm -rf bin dist
