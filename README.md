Balena extension runtime
========================

An OCI runtime for balenaOS hostapp extensions. It implements the OCI runtime
commands `create`, `start`, `kill`, `delete` and `state`. It runs no
long-lived process: an extension is an overlay that the host applies at boot,
and its container exits.

The containerd shim runs the runtime. Users do not run it. The same binary is
also `balena-extension-manager`, a hard link that selects its commands from
`argv[0]`. The manager does maintenance outside the OCI lifecycle.

[docs/extension-lifecycle.md](docs/extension-lifecycle.md) describes the full
flow: the container lifecycle, the proxy that carries the verdict, the labels,
volume fabrication, kernel overrides and withdrawal. Read it before you change
any of them.

## Build

```bash
make build   # static binary, no CGO, plus the balena-extension-manager link
make test    # unit tests
make vet     # static analysis
```

Build the binary before you run the e2e tests:

```bash
make build && go test -v ./e2e/
```

The integration tests run under docker compose:

```bash
make test-integration
```

## Manager commands

| Command | When it runs |
|---|---|
| `cleanup` | Every boot, after the engine starts. It removes garbage containers and the fabricated volumes that no container claims. |
| `cleanup --stale-os` | After a host OS update commits. It also removes the containers and images that the running system does not satisfy. |
| `validate` | Every boot, from `extension-rollback.service`. It judges an armed kernel override. |
| `hup commit`, `hup reject` | From meta-balena's `rollback-health`, in a host OS update window. |

The `validate` unit sets the healthcheck waits with `--settle`, `--retry` and
`--attempts`.

Do not run `cleanup --stale-os` or `hup` outside its window. Read the lifecycle
document first.

## Concurrency

The code does not show which process calls it. So it does not show which
operations can run at the same time. Read this section before you add a lock
or a defensive check, or review code for a race.

### Helios gates the runtime

Only helios deploys extensions. The legacy supervisor refuses them. Helios
creates the container with no restart policy, starts it and waits for it to
exit. The engine runs `create` and `start` one time for each container.

Helios waits to deploy, redeploy or remove an extension while
`rollback-health` or `extension-rollback` runs or has a queued start job. The
wait covers the full override trial. See `host_validating` in
`helios-balenahup/src/read.rs`.

| Entry point | Called by | Can overlap |
|---|---|---|
| runtime `create`, `start` | the engine, for a helios deploy | boot-time `cleanup` |
| `validate` | `extension-rollback.service` | boot-time `cleanup` |
| `hup commit`, `hup reject`, `cleanup --stale-os` | `rollback-health` | boot-time `cleanup` |
| `cleanup` | `hostapp-extensions-cleanup.service` | all of the above |

Runtime `create` and `start` never overlap `validate` or the `hup` commands. Do
not add code for a deploy during validation.

This section does not cover two cases:

- Helios reads a unit that it cannot query as not running. Thus an OS without
  the unit does not stop helios.
- An operator can run `balena run --runtime extension`. Helios does not see
  that container.

### The operation lock

`WithOperationLock` in `internal/manager/lock.go` is a flock on `/run` that
all processes share. It prevents one race: runtime `create` against boot-time
`cleanup`. Create-or-get can reuse a volume that already exists. Without the
lock, `cleanup` can remove that volume before `start` publishes its kernel.

### How helios reads a container

| Engine state | Helios result |
|---|---|
| `Exited (0)` | activated |
| `Created`, or a failed start with an exit code other than 126 or 127 | retry |
| any other exit code | failed, and the deploy stops the host update |

Thus helios retries a `start` that returns an error. The proxy exit status is
the verdict only when `start` returns success.

## Requirements

- Go 1.22 or later
- Linux, for the syscall signals and the process management
