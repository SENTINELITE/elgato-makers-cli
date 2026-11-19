# CI and release binaries pin Go 1.27.4 (.github/workflows). Set GO to a
# specific toolchain locally, e.g. GO=go1.27.4, to match it.
GO ?= go
# Use a version tag on this commit, or let the Go source supply the development version.
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --exact-match --dirty --match 'v[0-9]*.[0-9]*.[0-9]*' 2>/dev/null))
INSTALL_DIR ?= $(shell $(GO) env GOPATH)/bin

.PHONY: build test vet install

build:
	mkdir -p bin
	$(GO) build -trimpath $(if $(strip $(VERSION)),-ldflags "-X github.com/SENTINELITE/elgato-makers-cli/internal/cli.Version=$(VERSION)") -o bin/maker ./cmd/maker

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

install: build
	mkdir -p "$(INSTALL_DIR)"
	install -m 755 bin/maker "$(INSTALL_DIR)/maker"
