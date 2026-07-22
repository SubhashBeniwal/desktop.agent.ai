BINARY := aio-daemon
PKG := ./cmd/daemon
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build build-server build-relay run run-server run-relay tidy fmt vet test clean install

all: build build-server build-relay

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

# Development control-plane test harness.
build-server:
	CGO_ENABLED=0 go build -o bin/aio-server ./cmd/server

run: build
	./bin/$(BINARY) -config config.yaml

# Run the test harness on :8080, then point the daemon at ws://localhost:8080/agent
run-server: build-server
	./bin/aio-server

# Cross-network broker: daemons connect to /agent, control apps to /control.
build-relay:
	CGO_ENABLED=0 go build -o bin/aio-relay ./cmd/relay

run-relay: build-relay
	./bin/aio-relay

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

# Install to ~/bin (make sure it's on your PATH).
install: build
	install -m 0755 bin/$(BINARY) $(HOME)/bin/$(BINARY)

clean:
	rm -rf bin
