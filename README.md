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
# Build the binary (statically linked, no CGO).
# Also creates the balena-extension-manager hard link.
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

Integration tests run under docker compose:

```bash
make test-integration
```

## Usage

The runtime follows the standard OCI container lifecycle:

1. `create` — Reads `config.json`, validates extension labels, runs the
   `hooks/create` hook, spawns a proxy process, and writes OCI state
2. `start` — Runs the `hooks/start` hook and signals the proxy with the
   outcome: SIGUSR1 if the extension activated, SIGUSR2 if it refused. The
   container transitions to `stopped` either way
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

- **SIGUSR1** — activation succeeded, exit 0 (container shows "Exited (0)")
- **SIGUSR2** — the extension refused the activation, exit 1 (container shows
  "Exited (1)")
- **SIGTERM/SIGINT** — killed, exit cleanly

The proxy's exit status is what the engine records as the container's verdict,
which is how a refusal reaches the caller without failing the `start` call.

### Extension labels

Extensions are identified by OCI annotations (image labels):

| Label                              | Required | Description                                  |
|------------------------------------|----------|----------------------------------------------|
| `io.balena.image.class`           | yes      | Must be `overlay`                            |
| `io.balena.image.kernel-version`  | no       | Kernel ABI version (M.m.p) for userspace compatibility |
| `io.balena.image.kernel-abi-id`   | no       | Kernel binary interface identifier for module/eBPF compatibility |
| `io.balena.image.os-version`      | no       | HUP-commit retention predicate: comma-separated shell globs matched against `/etc/os-release` `VERSION_ID` |

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

#### Where an extension declines activation

`hooks/start` is the only rejection point. An extension that inspects the host
and decides it must not activate exits non-zero there: the runtime records the
container as `Exited (1)` and reports the `start` call as successful, so the
caller stops rather than retrying a decision that will not change.

`hooks/create` runs before the proxy process exists, so there is no container
status to carry a verdict.

### State management

OCI state is persisted as JSON under
`$XDG_RUNTIME_DIR/balena-extension-runtime/<container-id>/state.json`.
Writes use atomic rename for crash safety.

## Manager

The manager command (`balena-extension-manager`) runs outside the OCI
lifecycle. It is invoked from HUP hooks and ad-hoc maintenance. The binary
is a hard link to `balena-extension-runtime` and dispatches on `argv[0]`.

The manager's two cleanup calls are split by what each one can reach, not by
object type and not by update window.

### `cleanup`

Runs on every boot, with the engine up. It removes the extension containers the
engine calls garbage (dead, or a failed runtime create) and the fabricated
`ext_*` volumes no surviving container claims. Safe to run at any time.

```
balena-extension-manager cleanup
```

The volume sweep takes its snapshot of the volumes before it lists the
containers, and that order is the whole in-use proof: everything in the sweep
set was on disk before the claim query ran, so a deploy landing mid-sweep
writes a volume the set does not hold. A claim query it cannot complete, a
container that fabricates a volume but carries no image id, abandons the sweep
and fails the call rather than collecting against a short answer.

### `cleanup --stale-os`

Post-commit cleanup: additionally removes containers whose `kernel-version` or
`kernel-abi-id` labels mismatch the running kernel, and extension images whose
`io.balena.image.os-version` label doesn't match `/etc/os-release`
`VERSION_ID`. Volumes are not in this pass.

This flag is safe **only after** the HUP rollback-health commit. Outside that
window, a stale image is the rollback target and must be preserved.

It is not the primary convergence path for containers. The device agent already
drops a container whose claim the running kernel did not honour, at its next
poll and with no window gating. This pass is the orphan net for extensions the
agent cannot see: a manual deploy, or a release it no longer tracks. Images are
the part only this pass can do.

### `os-version` label grammar

- Value is a comma-separated list of shell-style globs
  (`filepath.Match` semantics).
- An image is retained if **any** pattern matches the running
  `VERSION_ID`, or if the label is absent/empty (legacy-safe default).
- Examples:
  - `2.119.0` — exact match, drops on any other version.
  - `2.119.*` — retains across patch or suffix bumps (`2.119.0-staging`,
    `2.119.1+rev1`, etc.).
  - `2.119.*,2.120.*` — builder opts in to one minor version of forward
    compat.

Note that `filepath.Match`'s `*` matches `.`, so `2.119.*` also matches
`2.119.0-staging` and similar suffixed versions — this is intentional.

## Requirements

- Go 1.22+
- Linux (uses syscall signals and process management)
