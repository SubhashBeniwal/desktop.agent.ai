GO ?= go
BINARY := aio-daemon
PKG := ./cmd/daemon
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build build-server build-relay build-desktop run run-server run-relay run-desktop package-macos package-windows icons tidy fmt vet test clean install

all: build build-server build-relay

build:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

# Development control-plane test harness.
build-server:
	CGO_ENABLED=0 $(GO) build -o bin/aio-server ./cmd/server

run: build
	./bin/$(BINARY) -config config.yaml

# Run the test harness on :8080, then point the daemon at ws://localhost:8080/agent
run-server: build-server
	./bin/aio-server

# Cross-network broker: daemons connect to /agent, control apps to /control.
build-relay:
	CGO_ENABLED=0 $(GO) build -o bin/aio-relay ./cmd/relay

run-relay: build-relay
	./bin/aio-relay

# Desktop GUI (menu-bar/tray app embedding the daemon). Needs cgo on macOS.
build-desktop:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o bin/aio-desktop ./cmd/desktop

run-desktop: build-desktop
	./bin/aio-desktop

# Installers -> dist/. The DMG must be built on macOS; the Windows installer
# cross-compiles anywhere with makensis installed (brew install makensis).
package-macos:
	GO=$(GO) packaging/macos/build-dmg.sh $(VERSION)

package-windows:
	GO=$(GO) packaging/windows/build-installer.sh $(VERSION)

# Regenerate cmd/desktop/assets icons.
icons:
	$(GO) run ./tools/icongen

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# Install to ~/bin (make sure it's on your PATH).
install: build
	install -m 0755 bin/$(BINARY) $(HOME)/bin/$(BINARY)

clean:
	rm -rf bin dist
