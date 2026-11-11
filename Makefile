BINARY := balena-extension-runtime
LINK := balena-extension-manager
MODULE := github.com/balena-os/balena-extension-runtime
VERSION ?= dev
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# The boot partitions, by label. The runtime follows each label's by-state
# link to its mount, so no mountpoint is linked in. An empty value from the
# command line beats ?=, so refuse it here rather than link a value that only
# fails on a device.
NONENC_BOOT_LABEL ?= balena-boot
ifeq ($(strip $(NONENC_BOOT_LABEL)),)
$(error NONENC_BOOT_LABEL is empty; the recipe must pass BALENA_NONENC_BOOT_LABEL)
endif
ENC_BOOT_LABEL ?= resin-boot
ifeq ($(strip $(ENC_BOOT_LABEL)),)
$(error ENC_BOOT_LABEL is empty; the recipe must pass BALENA_ENC_BOOT_LABEL)
endif

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT) \
	-X $(MODULE)/internal/bootenv.nonencBootLabel=$(NONENC_BOOT_LABEL) \
	-X $(MODULE)/internal/bootenv.encBootLabel=$(ENC_BOOT_LABEL)

# $(BINARY) is phony rather than a file target with sources listed: go build
# does its own staleness check, and a file target with no prerequisites is
# never remade, which silently hands `make build && go test ./e2e/` the
# previous binary to test.
.PHONY: build clean test test-integration vet $(BINARY)

build: $(BINARY)
	ln -f $(BINARY) $(LINK)

$(BINARY):
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $@ ./cmd/$@/

clean:
	rm -f $(BINARY) $(LINK)

test:
	go test -v -race ./internal/... ./cmd/...

test-integration:
	docker compose -f docker-compose.test.yml up --build --abort-on-container-exit --exit-code-from sut

vet:
	go vet ./...
