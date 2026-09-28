# Extension lifecycle

An extension is an overlay image. mobynit applies it at boot. The containerd
shim runs the runtime, and nothing else calls the runtime. This document
describes the full flow. It starts when the engine creates a container. It ends
when the manager removes what a withdrawn extension left.

The initramfs `kexec` script and `extension-rollback.service` are in
meta-balena. This document describes them from the comments in this
repository, not from those files.

## The container lifecycle

The runtime implements the five OCI commands:

1. `create` does these steps in sequence:
   1. It reads the bundle spec and the image labels.
   2. It checks the class.
   3. For a kernel override, it creates and fills the `/boot` volume.
   4. It starts the proxy, waits until the proxy is ready for signals, and
      writes the OCI state.
   5. It records the volume name beside the state.
2. `start` calls `activate`. `activate` activates a kernel override if the
   extension claims one. Then `start` signals the proxy with the result.
   - A verdict from `activate` stops the container with `Exited (1)`. `start`
     still returns success.
   - Any other error fails the call. The container stays `created`, and
     containerd can retry.
3. `kill` signals the proxy.
4. `delete` removes the runtime state. It reads no bundle spec and resolves no
   rootfs. Thus a bundle that is already removed cannot make `delete` fail.
   `delete --force` first kills a running or created container.
5. `state` prints the OCI state as JSON.

### Extensions exit

The runtime never runs content from the image. No process stays alive. The
container holds only the image reference and the verdict. mobynit applies the
extension content at boot. `start` moves the container to `stopped` on
purpose. A container that shows `Exited` is the normal result, not a failure.

### The proxy carries the verdict

No extension process runs, so `create` starts a proxy process only to give
containerd a PID. The proxy exit status is the verdict of the extension. It is
the only way that a refusal gets back to the caller.

| Signal | Exit | Engine shows |
|---|---|---|
| SIGUSR1 | 0 | `Exited (0)`, activated |
| SIGUSR2 | 1 | `Exited (1)`, the extension refused |
| SIGTERM, SIGINT | 0 | killed, clean stop |

This rule controls the error handling in `start`:

- Only activation gives a verdict. When `activate` refuses an image, `start`
  succeeds and the container fails. Nothing retries a decision that cannot
  change.
- Every other failure fails the call, so containerd can retry. Examples are an
  unreadable spec, an unmounted state partition, a missing volume record and a
  failed arm.

## Labels

OCI annotations identify an extension. The image carries them as labels.

| Label | Required | Meaning |
|---|---|---|
| `io.balena.image.class` | yes | Must be `overlay`. The runtime checks only this label. |
| `io.balena.image.kernel-version` | no | The kernel version (M.m.p) for userspace compatibility. |
| `io.balena.image.kernel-abi-id` | no | The kernel binary interface id. It causes volume fabrication and override activation. |
| `io.balena.image.os-version` | no | The retention rule: comma-separated globs that the manager compares with `VERSION_ID` in `/etc/os-release`. |
| `io.balena.service-name` | no | The compose service. The volume name uses it. |

balena-engine does not copy container labels into the OCI spec annotations.
The runtime reads the labels and the image id from the container store of the
engine. When no store is available, the spec annotations are the identity, and
the image id is empty. `create` selects the source one time, so the labels and
the image id always come from the same source.

### `os-version` grammar

The value is a comma-separated list of shell-style globs. The manager keeps an
image in two cases:

- A pattern matches the running `VERSION_ID`.
- The label is absent or empty.

Examples:

- `2.119.0` keeps one version.
- `2.119.*` keeps all patch and suffix versions of 2.119.
- `2.119.*,2.120.*` also keeps the next minor version.

`*` matches `.`, so `2.119.*` also keeps `2.119.0-staging`. The manager keeps
it on purpose.

## Volume fabrication

An extension that claims `kernel-abi-id` gets a `/boot` volume. `create`
creates and fills the volume. During `start`, `activate` publishes the kernel
from that volume, so the volume must be full before `start`.

The engine cannot make this volume. It resolves mounts before it runs the
runtime. Thus a runtime cannot attach a named, labelled volume to its own
container. The runtime does not attach the volume either. `create` records the
volume name beside the container state, and `activate` reads it.

The volume name is its identity: `ext_<service>_<image>_boot`.

- `<service>` is the service label. When the label is absent, it is a prefix
  of the container id.
- `<image>` comes from the image id.

The engine creates the volume, or returns it if it exists. Thus a redeploy
reuses the volume and does not make a second one. `cleanup` derives the same
name to find the volumes that it must keep. The runtime and the manager share
this naming rule. If the two derive different names, the sweep does not find a
live volume, and it removes that volume.

## State

The runtime writes the OCI state as JSON to `<root>/<container-id>/state.json`.
containerd sets `<root>` with `--root`. Without `--root`, the root is
`$XDG_RUNTIME_DIR/balena-extension-runtime`, or
`/run/balena-extension-runtime` when `XDG_RUNTIME_DIR` is not set.

## Kernel overrides

An extension with the `kernel-abi-id` label contains a kernel that replaces the
rootfs kernel:

1. The runtime arms the kernel during `start`.
2. The initramfs boots the kernel.
3. `balena-extension-manager validate` judges the kernel on the next boot.

### Records

No single record holds the override state. Each boot builds the state again
from the records below. An ABI is a checksum of the kernel image. Thus
`override-rejected` needs no expiry: a line stops to apply when the extension
contains different kernel bytes.

| Record | Written by | Location |
|---|---|---|
| `kernel_override_abi` (the arm) | activation, validation, `hup reject` | boot environment block |
| `kernel_override_abi_committed_{A,B}` | validation, `hup commit` | boot environment block |
| `kernel_override_trial` | initramfs stage 2 | boot environment block |
| `kernel_override_abi_rejected` (the relay) | `hup reject`, read on the next boot | boot environment block |
| `upgrade_available` | the host OS update path, read only here | boot environment block |
| `boot-by-abi/<abi>` | activation, validation | `/mnt/data`, read by the initramfs |
| `extension-health-variables` | activation, `rollback-tests` | `/mnt/state`. When it exists, a window is open. |
| `override-rejected` | validation | `/mnt/state`. Validation adds lines and never removes them. |
| the ABI claim | a deployed extension image | the container store on disk |
| the running ABI | the initramfs, through `/proc/cmdline` | memory only |

### Activation

`start` calls `activate` before it stops the container.

- A verdict marks the container `Exited (1)`, and `start` succeeds.
- A machine condition fails the call, and containerd retries.

This split keeps a machine condition off the extension record. Activation
checks the image before the machine. Thus an image defect is a verdict on
every machine.

| Check | Failure class |
|---|---|
| A regular file under `/boot` in the image hashes to the ABI | verdict |
| The image contains `Module.symvers` under `{usr/,}lib/modules` | verdict |
| `/mnt/state` is mounted | machine |
| The ABI is not in `override-rejected` | verdict |
| The fabricated volume is recorded, exists and contains the kernel | machine |

Do not add the mount check to validation. Validation already runs only with
`/mnt/state` mounted:

- Its unit starts after `resin-state.service`.
- A breadcrumb on that partition gates the `hup` subcommands.

With the check, a device without the mount would loop:

1. The write of the rejection record fails before validation records a verdict.
2. `hup reject` returns before its reboot.
3. The armed kernel already runs, so the boot spends no trial count.
4. The healthcheck cycle repeats on every boot.

Three writes follow, under the operation lock. Each write is durable before
the next write names it:

1. Publish `boot-by-abi/<abi>`.
2. Write the health prestate.
3. Arm. This opens the validation window.

The link points to `../docker/volumes/<name>/_data/<kernel>`. The link target
and the volume path both come from the volume name. They never come from the
mountpoint that the engine reports. The docker root is a bind of
`/mnt/data/docker`. A path from that mountpoint resolves in the running OS,
but not in the initramfs. The `override` package builds the target, because it
owns the directory of the link.

### The boot decision

`validate` runs from `extension-rollback.service` on every boot. For an armed
override, it does one of three things:

- It commits the override.
- It rejects the override.
- It keeps the override pending for another boot.

When the armed kernel booted, validation judges it immediately. It waits for
the system to settle and runs `rollback-tests`. Then it commits, or it rejects
with `by=health`.

When the armed kernel did not boot, validation usually keeps it pending,
because it tested nothing. The trial count is the only way out. Stage 2 stops
to offer an arm when the count reaches the limit. Without a rejection with
`by=boots`, an override that never boots would stay pending for the life of
the device. The initramfs script has a copy of the limit, `TRIAL_LIMIT`. Both
copies ship in one rootfs image.

Two steps come first, before validation derives the running slot:

1. Validation reads and removes a rejection that a host OS rollback relayed.
2. Validation sweeps every recorded ABI that no deployed extension claims.

A device that cannot name its slot still gets both steps.

In a host OS update window, the update path sets `upgrade_available`.
Validation then returns and judges nothing.

### Verdicts

Each verdict is one block write under the operation lock. Validation never
holds the lock during the settle wait or the retry waits. With the default
flags, these waits total fifteen minutes. Holding the lock would stop
`hostapp-extensions-cleanup.service` for every trial.

| Verdict | Block write | After the write |
|---|---|---|
| commit | Set `committed[slot] = abi`. Clear the trial count. | Remove the prestate. |
| reject | First write `override-rejected` and the audit line, both fsynced. Then clear the arm, restore `committed[slot]` and clear the count. | Unpublish the kernel. Remove the prestate. `by=health` also reboots. |
| restore | Set `arm = committed[slot]`. | Nothing. No window opened. |
| forget | Remove the arm and each committed value that names the ABI. | Unpublish. Remove the prestate if the write cleared the arm. |

A rejection writes its records before the block write. When helios redeploys
the same image, `override-rejected` refuses the arm. If a crash lost that
record after the block write cleared the arm, the same kernel bytes could be armed again.
No record would show that they failed.

Commit, reject and restore write nothing when the arm changed during the
trial, because that arm owns its own boot. A rejection still writes its
records and unpublishes the kernel, because the judged kernel bytes failed.

The reboot uses `systemctl reboot`, not `reboot(2)`. The clean shutdown
flushes the journal.

### The host OS update window

In this window, validation returns because the update path set
`upgrade_available`.
meta-balena's `rollback-health` calls the verdicts directly.

`hup commit` records the running kernel as the proven override of the running
slot. It does this only when the arm took effect. It keeps the prestate for the
next arm.

`hup reject` does three things:

- It replaces the arm with the committed override of the slot that the
  rollback goes to.
- It relays the ABI to the next boot, because the reboot follows immediately.
  The next boot removes the records.
- It records no rejection. The rootfs rolled back, so the kernel bytes were
  not proven bad.

When the arm is already the committed value of the target slot, `hup reject`
relays nothing. Otherwise the next boot would forget a proven ABI.

Neither command takes a slot argument. Each derives the slot from
`/mnt/sysroot/active`. A write to the committed value of the wrong slot
removes a proven kernel.

### The record sweep

The sweep forgets every recorded ABI that no deployed extension claims. The
sweep is not a verdict. The same ABI arms again without problems on a
redeploy.

The claims come from the container store on disk, not from the engine. The
engine can be stopped when the unit runs. Also, the answer must agree with the
initramfs, which reads the same store. On any error, the sweep does nothing.
If it read an error as an empty claim set, it would forget every record on a
data partition that is not yet populated.

- The sweep reads the recorded set before any claim query, and never reads it
  again. A container claims its ABI from creation. Thus every record is older
  than every claim snapshot.
- The sweep queries the claims one time, inside the operation lock. This
  covers a withdrawal and then a redeploy of the same ABI, because `create`
  takes the same lock around volume fabrication.
- The block write comes before the unpublish. The next sweep removes a link
  that no record names. The opposite order costs a boot.

The sweep clears an arm but does not restore it, because it runs before
validation can name the slot. A later step points the empty arm back at the
committed override of the slot. An empty arm is not a verdict. Thus an empty
arm at that step can only be an arm that the sweep cleared, beside a proven
override that it kept.

## Withdrawal

To withdraw an extension, remove its container. No other step is necessary.
Nothing disarms an override:

- The `Dead` flag of the container tells the initramfs to exclude the mount.
- The sweep on the next boot removes the publications that the extension left.

A removal that leaves the container `dead` counts as a success. The engine
cannot release a layer that the running root uses.

`balena-extension-manager cleanup` runs on every boot after the engine starts.
It removes two kinds of items:

- The containers that the engine reports as garbage: dead containers, and
  containers whose runtime create failed.
- The fabricated volumes that no remaining container claims.

The volume sweep lists the volumes before it lists the containers. This order
is the full proof that no removed volume is in use. Every volume in the sweep
set existed before the claim query ran. Thus a deploy during the sweep writes a
volume that is not in the set. A fabricated volume is never attached, so the
engine never protects it as in use.

The sweep stops and the call fails in two cases:

- The claim query cannot complete.
- A container fabricates a volume but has no image id.

In both cases the claim set could be short, and the sweep does not remove
volumes against it.

`cleanup --stale-os` also removes the containers and images whose
`kernel-abi-id`, `kernel-version` or `os-version` label the running system does
not satisfy. Use it only after a host OS update commits. Before the commit, a
stale image is the rollback target.

This pass does not remove volumes, on purpose. A withdrawn override on a
device that never changes its OS leaves a volume that never becomes stale. A
volume sweep that waited for staleness would keep that kernel forever.

For containers, this pass is not the main path to the target state. The
device agent already removes a container whose claim the running kernel did
not honour. It does this at its next poll, with no window. This pass removes
orphans that the agent cannot see, such as a manual deploy or a release that
the agent no longer tracks. Only this pass removes images.
