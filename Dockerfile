# Stage 1: build binaries and compile test binaries
FROM golang:1.22-alpine AS gobuild

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 go build -ldflags '-s -w' -o balena-extension-runtime ./cmd/balena-extension-runtime/
RUN ln -f balena-extension-runtime balena-extension-manager
RUN CGO_ENABLED=0 go test -c -o integration.test ./integration/
RUN CGO_ENABLED=0 go test -c -o e2e.test ./e2e/

# Stage 2: test image with Docker-in-Docker
FROM docker:25-dind AS testimage

RUN apk add --no-cache bash

COPY --from=gobuild /src/balena-extension-runtime /src/balena-extension-runtime
COPY --from=gobuild /src/balena-extension-manager /src/balena-extension-manager
COPY --from=gobuild /src/integration.test /src/integration.test
COPY --from=gobuild /src/e2e.test /src/e2e.test

# Register extension runtime so containers created with --runtime=extension
# go through our binary.
RUN mkdir -p /etc/docker && \
    echo '{"runtimes":{"extension":{"path":"/src/balena-extension-runtime"}}}' > /etc/docker/daemon.json

# Place e2e test binary in /src/e2e/ so the relative path ../balena-extension-runtime resolves.
RUN mkdir -p /src/e2e && cp /src/e2e.test /src/e2e/e2e.test

# Disable TLS so dockerd listens on unix socket only and docker CLI can connect.
ENV DOCKER_TLS_CERTDIR=

COPY <<'ENTRYPOINT' /src/run-tests.sh
#!/bin/bash
set -e

# Start dockerd in background (no TLS).
dockerd-entrypoint.sh &

# Wait for Docker daemon to be ready.
# Poll via socket existence + `docker version` (not `docker info`): concurrent
# `docker info` probes on Alpine/busybox contend with dockerd's own startup
# (spawning heavy Go binaries during the daemon's init races with it), and
# dockerd never completes listener setup. Lighter probes avoid the contention.
export DOCKER_HOST=unix:///var/run/docker.sock
echo "Waiting for Docker daemon..."
for i in $(seq 1 60); do
    if [ -S /var/run/docker.sock ] && docker version --format '.' >/dev/null 2>&1; then
        echo "Docker daemon ready."
        break
    fi
    if [ "$i" -eq 60 ]; then
        echo "Timed out waiting for Docker daemon."
        exit 1
    fi
    sleep 1
done

echo "=== Running integration tests ==="
/src/integration.test -test.v

echo "=== Running e2e tests ==="
cd /src/e2e && ./e2e.test -test.v

echo "=== All tests passed ==="
ENTRYPOINT

RUN chmod +x /src/run-tests.sh

CMD ["/src/run-tests.sh"]
