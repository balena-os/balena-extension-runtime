BINARY := balena-extension-runtime
LINK := balena-extension-manager
MODULE := github.com/balena-os/balena-extension-runtime
VERSION ?= dev
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# The non-encrypted boot partition's mountpoint. The recipe passes its own
# BALENA_NONENC_BOOT_MOUNT. An empty value from the command line beats ?=, and
# would link an empty path that only fails on a device, so refuse it here.
BOOT_MOUNT ?= /mnt/boot
ifeq ($(strip $(BOOT_MOUNT)),)
$(error BOOT_MOUNT is empty; the recipe must pass BALENA_NONENC_BOOT_MOUNT)
endif

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.GitCommit=$(GIT_COMMIT) \
	-X $(MODULE)/internal/bootenv.bootMount=$(BOOT_MOUNT)

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
