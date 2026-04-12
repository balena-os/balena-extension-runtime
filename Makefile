BINARY := balena-extension-runtime
LINK := balena-extension-manager
MODULE := github.com/balena-os/balena-extension-runtime
VERSION ?= dev
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT)

.PHONY: build clean test test-integration vet

build: $(BINARY)
	ln -f $(BINARY) $(LINK)

$(BINARY):
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $@ ./cmd/$@/

clean:
	rm -f $(BINARY) $(LINK)

test:
	go test -v -race ./internal/...

test-integration:
	docker compose -f docker-compose.test.yml up --build --abort-on-container-exit --exit-code-from sut

vet:
	go vet ./...
