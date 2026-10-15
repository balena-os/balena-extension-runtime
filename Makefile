BINARY := balena-extension-runtime
LINK := balena-extension-manager
MODULE := github.com/balena-os/balena-extension-runtime
VERSION ?= dev
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# The two mountpoints the block can live on. They differ only where the boot
# partition is split. An empty value from the command line beats ?=, so refuse
# it here rather than link a path that only fails on a device.
BOOT_MOUNT ?= /mnt/boot
ifeq ($(strip $(BOOT_MOUNT)),)
$(error BOOT_MOUNT is empty; the recipe must pass BALENA_BOOT_MOUNT)
endif
NONENC_BOOT_MOUNT ?= $(BOOT_MOUNT)
ifeq ($(strip $(NONENC_BOOT_MOUNT)),)
$(error NONENC_BOOT_MOUNT is empty; the recipe must pass BALENA_NONENC_BOOT_MOUNT)
endif

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT) \
	-X $(MODULE)/internal/bootenv.bootMount=$(BOOT_MOUNT) \
	-X $(MODULE)/internal/bootenv.nonencBootMount=$(NONENC_BOOT_MOUNT)

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
