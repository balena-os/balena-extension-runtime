BINARY := balena-extension-runtime
MODULE := github.com/balena-os/balena-extension-runtime
VERSION ?= dev
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT)

.PHONY: build clean test vet

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/balena-extension-runtime/

clean:
	rm -f $(BINARY)

test:
	go test -v -race ./internal/...

vet:
	go vet ./...
