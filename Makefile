# tcp-commander build
#
# Common targets:
#   make            # build daemon for the host
#   make linux      # cross-compile linux/amd64 + linux/arm64
#   make test       # go test ./...
#   make vet        # go vet ./...
#   make clean      # remove build outputs
#   make install    # install host binary to $(PREFIX)/bin
#
# Override:
#   make VERSION=1.2.3
#   make PREFIX=/opt/tcp-commander install

GO          ?= go
PREFIX      ?= /usr/local
BIN_DIR     := bin
DIST_DIR    := dist

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X github.com/yuanmomo/tcp-commander/internal/server.Version=$(VERSION)

DAEMON      := tcpcommanderd

GOFILES     := $(shell find . -type f -name '*.go' -not -path './$(BIN_DIR)/*' -not -path './$(DIST_DIR)/*')

.PHONY: all build linux linux-amd64 linux-arm64 clean test vet fmt install help

all: build

build: $(BIN_DIR)/$(DAEMON)

$(BIN_DIR)/$(DAEMON): $(GOFILES) go.mod go.sum
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $@ ./cmd/$(DAEMON)

# Cross-compile release binaries for the typical Linux server targets.
linux: linux-amd64 linux-arm64

linux-amd64:
	@mkdir -p $(DIST_DIR)/linux-amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST_DIR)/linux-amd64/$(DAEMON) ./cmd/$(DAEMON)

linux-arm64:
	@mkdir -p $(DIST_DIR)/linux-arm64
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST_DIR)/linux-arm64/$(DAEMON) ./cmd/$(DAEMON)

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

install: build
	install -Dm755 $(BIN_DIR)/$(DAEMON) $(DESTDIR)$(PREFIX)/bin/$(DAEMON)

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)

help:
	@grep -E '^[a-zA-Z_-]+:.*' Makefile | grep -v '^\.PHONY' | sort
