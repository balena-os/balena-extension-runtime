Balena extension runtime
========================

An OCI-compliant container runtime for balenaOS hostapp extensions. It
implements the OCI runtime spec interface (`create`, `start`, `kill`,
`delete`, `state`) but instead of running long-lived processes, it executes
overlay-based extensions that apply filesystem changes to the host and exit
immediately.

The runtime is invoked by containerd as a shim — it is not called directly
by users.

## Build

```bash
# Build the binary (statically linked, no CGO)
make build

# Run unit tests
make test

# Run static analysis
make vet
```

E2E tests require the binary to be built first:

```bash
make build && go test -v ./e2e/
```

## Usage

The runtime follows the standard OCI container lifecycle:

1. `create` — Reads `config.json`, validates extension labels, runs the
   `hooks/create` hook, spawns a proxy process, and writes OCI state
2. `start` — Runs the `hooks/start` hook, signals the proxy to exit cleanly
   (SIGUSR1), and transitions the container to `stopped`
3. `kill` — Sends a signal to the proxy process
4. `delete` — Runs the `hooks/delete` hook and removes runtime state
5. `state` — Returns OCI state JSON to stdout

### Why extensions exit immediately

Unlike traditional runtimes, extensions don't run persistent processes. They
apply overlay filesystem changes during their hooks and then exit. The `start`
command intentionally transitions the container to `stopped` — this is by
design.

### Proxy process

The runtime spawns a proxy subprocess (`balena-extension-runtime proxy`)
during `create` to give containerd a real PID to track between `create` and
`start`. The proxy blocks on signals:

- **SIGUSR1** — "start complete", exit cleanly (container shows "Exited (0)")
- **SIGTERM/SIGINT** — killed, exit cleanly

### Extension labels

Extensions are identified by OCI annotations (image labels):

| Label                              | Required | Description                                  |
|------------------------------------|----------|----------------------------------------------|
| `io.balena.image.class`           | yes      | Must be `overlay`                            |
| `io.balena.image.kernel-version`  | no       | Kernel ABI version (M.m.p) for userspace compatibility |
| `io.balena.image.kernel-abi-id`   | no       | Kernel binary interface identifier for module/eBPF compatibility |

The runtime acts on the labels above. Any other annotation under the
`io.balena.image.*` prefix is opaque to the runtime but is still forwarded to
hooks as an environment variable (see below).

### Extension hooks

Extensions can ship executable scripts at `<rootfs>/hooks/{create,start,delete}`.
Hooks receive the following environment variables:

- `EXTENSION_ROOTFS` — absolute path to the extension rootfs
- `EXTENSION_IMAGE_*` — every annotation under the `io.balena.image.*` prefix
  is forwarded as `EXTENSION_IMAGE_<NAME>`, with dashes converted to
  underscores and uppercased (e.g., `io.balena.image.kernel-abi-id` becomes
  `EXTENSION_IMAGE_KERNEL_ABI_ID`). The forwarding is prefix-based, so custom
  or future labels are available to hooks without runtime changes.

### State management

OCI state is persisted as JSON under
`$XDG_RUNTIME_DIR/balena-extension-runtime/<container-id>/state.json`.
Writes use atomic rename for crash safety.

## Requirements

- Go 1.22+
- Linux (uses syscall signals and process management)
