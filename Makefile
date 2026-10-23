# Builds skerry with its embedded guest agent, and signs it so it can start
# VMs through Apple's Virtualization framework.

GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARCH    := $(shell $(GO) env GOARCH)
AGENT   := internal/assets/files/skerry-agent-linux-$(ARCH)
LDFLAGS := -s -w -X github.com/skerrysoftware-rgb/skerry/internal/cli.Version=$(VERSION)

.PHONY: build agent test lint clean

build: agent
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/skerry ./cmd/skerry
ifeq ($(shell uname -s),Darwin)
	codesign --force --sign - --entitlements build/skerry.entitlements bin/skerry
endif

agent:
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o $(AGENT) ./cmd/skerry-agent

test:
	$(GO) test -race ./...

lint:
	$(GO) vet ./...
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 $(GO) vet ./internal/agent/ ./cmd/skerry-agent/
	test -z "$$(gofmt -l cmd internal)"

clean:
	rm -rf bin $(AGENT)
