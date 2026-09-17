package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/override"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var activateTestLogger = slog.New(slog.NewTextHandler(os.Stderr, nil))

// activateHost redirects every host path activation touches at a temporary
// tree, reports every partition mounted, and captures the arm instead of
// writing a boot environment block. It returns the tree's root.
func activateHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	prevBootByABI, prevState, prevVPN := override.BootByABIDir, override.StateMount, override.VPNActiveMarker
	prevEngine := override.DataEngineRoot
	prevMounted, prevArm := isMounted, armOverride

	override.BootByABIDir = filepath.Join(root, "mnt", "data", "boot-by-abi")
	override.DataEngineRoot = filepath.Join(root, "mnt", "data", "docker")
	override.StateMount = filepath.Join(root, "mnt", "state")
	override.VPNActiveMarker = filepath.Join(root, "run", "openvpn", "active")
	isMounted = func(string) (bool, error) { return true, nil }
	armOverride = func(string) error { return nil }

	require.NoError(t, os.MkdirAll(override.StateMount, 0o755))

	t.Cleanup(func() {
		override.BootByABIDir, override.StateMount, override.VPNActiveMarker = prevBootByABI, prevState, prevVPN
		override.DataEngineRoot = prevEngine
		isMounted, armOverride = prevMounted, prevArm
	})
	return root
}

// activateRootfs lays down an extension rootfs whose /boot holds a kernel
// image with the given content, and returns the rootfs and its ABI id.
func activateRootfs(t *testing.T, root, content string) (string, string) {
	t.Helper()
	rootfs := filepath.Join(root, "rootfs")
	require.NoError(t, os.MkdirAll(filepath.Join(rootfs, "boot"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootfs, "boot", "Image"), []byte(content), 0o644))
	modules := filepath.Join(rootfs, "usr", "lib", "modules", "6.6.20-test")
	require.NoError(t, os.MkdirAll(modules, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modules, "Module.symvers"), nil, 0o644))
	sum := sha256.Sum256([]byte(content))
	return rootfs, hex.EncodeToString(sum[:])
}

// fabricatedVolume records a filled volume for a container the way create
// would have, and returns its data directory.
func fabricatedVolume(t *testing.T, root, containerID, name string) string {
	t.Helper()
	dockerRoot(t, root)
	dataDir := filepath.Join(root, "var", "lib", "docker", "volumes", name, "_data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	// create fills the volume before start publishes the link.
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "Image"), nil, 0o644))
	require.NoError(t, oci.WriteBootVolume(containerID, name))
	return dataDir
}

// dockerRoot points the runtime at the engine root under a test's tree.
func dockerRoot(t *testing.T, root string) {
	t.Helper()
	oci.SetDockerRoot(filepath.Join(root, "var", "lib", "docker"))
	t.Cleanup(func() { oci.SetDockerRoot(defaultDockerRoot) })
}

// A userspace-only extension is not a kernel override and activation is a
// no-op for it. Nothing about the host is even read.
func TestActivate_NoLabelDoesNothing(t *testing.T) {
	root := activateHost(t)
	rootfs, _ := activateRootfs(t, root, "kernel")

	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class: labels.ClassOverlay,
	}))

	_, err := os.Stat(override.BootByABIDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "a userspace extension publishes nothing")
}

// The label is the claim and the shipped bytes verify it. Nothing under /boot
// hashing to the label means the image is not what it says it is, and no
// retry changes that.
func TestActivate_NoMatchingKernelIsDeclined(t *testing.T) {
	root := activateHost(t)
	rootfs, _ := activateRootfs(t, root, "kernel")

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	assert.ErrorIs(t, err, errVerdict)
}

// The bytes verified have to live inside the extension being verified.
func TestActivate_KernelOnlyAsASymlinkIsDeclined(t *testing.T) {
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	real := filepath.Join(rootfs, "boot", "Image")
	elsewhere := filepath.Join(root, "elsewhere")
	require.NoError(t, os.Rename(real, elsewhere))
	require.NoError(t, os.Symlink(elsewhere, real))

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	assert.ErrorIs(t, err, errVerdict)
}

// The image checks run before the mount check, so an image that fails its
// own check declines even on an unmounted state: a machine condition only
// matters once the image has passed what it alone can prove.
func TestActivate_AbsentBootIsDeclined(t *testing.T) {
	root := activateHost(t)
	rootfs := filepath.Join(root, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))
	isMounted = func(string) (bool, error) { return false, nil }

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	assert.ErrorIs(t, err, errVerdict)
}

// A /boot that cannot be read says nothing about the image, so it must not be
// recorded as the extension refusing. Only its absence is the image's fault.
func TestActivate_UnreadableBootIsRetryable(t *testing.T) {
	root := activateHost(t)
	rootfs := filepath.Join(root, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootfs, "boot"), nil, 0o644))

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVerdict)
}

// A kernel with no modules boots a device that has no drivers for its own
// hardware. The build refuses to produce one, and the claim query mobynit
// answers reads the label alone, so this is the only place that can catch it.
func TestActivate_NoModulesTreeIsDeclined(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")
	require.NoError(t, os.RemoveAll(filepath.Join(rootfs, "usr")))

	var armed int
	armOverride = func(string) error {
		armed++
		return nil
	}

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	assert.ErrorIs(t, err, errVerdict)
	assert.Zero(t, armed)
}

// The label the claim query cannot check. FilterByKernelVersion would drop
// this image after the kexec, leaving its kernel without drivers.
func TestActivate_KernelVersionMismatchIsDeclined(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:         labels.ClassOverlay,
		labels.KernelABIID:   abi,
		labels.KernelVersion: "5.15.0",
	})
	assert.ErrorIs(t, err, errVerdict)
}

// The release carries a local-version suffix the label does not, so only the
// M.m.p is compared.
func TestActivate_MatchingKernelVersionIsAccepted(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:         labels.ClassOverlay,
		labels.KernelABIID:   abi,
		labels.KernelVersion: "6.6.20",
	}))
}

// A modules tree reached through a symlinked release directory is still a
// modules tree; only the drivers' absence is the image's fault.
func TestActivate_SymlinkedModulesTreeIsAccepted(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	real := filepath.Join(rootfs, "usr", "lib", "modules", "6.6.20-test")
	elsewhere := filepath.Join(root, "modules-6.6.20-test")
	require.NoError(t, os.Rename(real, elsewhere))
	require.NoError(t, os.Symlink(elsewhere, real))

	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}))
}

// The published link names the kernel, so a volume that does not hold it
// would publish a dangling link. The fill is create's, not the image's.
func TestActivate_VolumeWithoutTheKernelIsRetryable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	dataDir := fabricatedVolume(t, root, "c1", "ext_test_abc_boot")
	require.NoError(t, os.Remove(filepath.Join(dataDir, "Image")))

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVerdict)
}

// A kernel the boot-time validator rejected must not go straight back. The
// record holds one bare ABI per line and the ABI is a hash of the image, so
// the entry stops applying as soon as the extension ships different bytes.
func TestActivate_RejectedABIIsDeclined(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")
	require.NoError(t, os.WriteFile(filepath.Join(override.StateMount, "override-rejected"),
		[]byte("1111111111111111111111111111111111111111111111111111111111111111\n"+abi+"\n"), 0o644))

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	assert.ErrorIs(t, err, errVerdict)
}

// A boot that races the state mount is a machine condition, not the extension
// declining. Recording it as a decline marks the extension as having refused,
// permanently, for something a later boot fixes.
func TestActivate_UnmountedStateIsRetryable(t *testing.T) {
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	isMounted = func(string) (bool, error) { return false, nil }

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVerdict)
}

// An unreadable rejection record cannot tell "listed" from "not listed", so
// it is a machine condition rather than a verdict.
func TestActivate_UnreadableRejectionRecordIsRetryable(t *testing.T) {
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	require.NoError(t, os.Mkdir(filepath.Join(override.StateMount, "override-rejected"), 0o755))

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVerdict)
}

func TestActivate_MissingVolumeRecordIsRetryable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVerdict)
}

// A volume the engine put somewhere else has no data directory under the
// docker root, and activate looks nowhere else. A later boot can fix that, so
// it is a machine condition.
func TestActivate_AbsentVolumeDirIsRetryable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	dockerRoot(t, root)
	require.NoError(t, oci.WriteBootVolume("c1", "ext_test_abc_boot"))

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVerdict)
}

func TestActivate_PublishesArmsAndRecordsThePrestate(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	var armed []string
	armOverride = func(a string) error {
		armed = append(armed, a)
		return nil
	}

	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}))

	// Relative to the data partition, as the initramfs resolves it.
	target, err := os.Readlink(filepath.Join(override.BootByABIDir, abi))
	require.NoError(t, err)
	assert.Equal(t, "../docker/volumes/ext_test_abc_boot/_data/Image", target)

	prestate, err := os.ReadFile(filepath.Join(override.StateMount, "extension-health-variables"))
	require.NoError(t, err)
	assert.Equal(t, "BALENAOS_ROLLBACK_VPNONLINE=0\n", string(prestate))

	assert.Equal(t, []string{abi}, armed)

	entries, err := os.ReadDir(override.BootByABIDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the temporary name must not survive")
	assert.Equal(t, abi, entries[0].Name())
}

// extension-rollback removes the prestate to close a window, so its presence
// is what says one is open. A half-written or empty file must never be what a
// validator finds there.
func TestActivate_PrestateIsPublishedWhole(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	path := filepath.Join(override.StateMount, "extension-health-variables")
	require.NoError(t, os.WriteFile(path, []byte("BALENAOS_ROLLBACK_VPNONLINE=1\n"), 0o644))

	// Only the rename may touch the published name.
	armOverride = func(string) error {
		entries, err := os.ReadDir(override.StateMount)
		require.NoError(t, err)
		for _, e := range entries {
			assert.NotContains(t, e.Name(), ".new", "the temporary name must not survive")
		}
		return nil
	}

	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}))

	prestate, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "BALENAOS_ROLLBACK_VPNONLINE=0\n", string(prestate))
}

func TestActivate_RecordsAReachableVPN(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")
	require.NoError(t, os.MkdirAll(filepath.Dir(override.VPNActiveMarker), 0o755))
	require.NoError(t, os.WriteFile(override.VPNActiveMarker, nil, 0o644))

	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}))

	prestate, err := os.ReadFile(filepath.Join(override.StateMount, "extension-health-variables"))
	require.NoError(t, err)
	assert.Equal(t, "BALENAOS_ROLLBACK_VPNONLINE=1\n", string(prestate))
}

// A redeploy of the same extension republishes the same link. Publish is
// idempotent because a retry is a recreate.
func TestActivate_RepublishIsIdempotent(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	annotations := map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}
	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, annotations))
	require.NoError(t, activate(context.Background(), activateTestLogger, "c1", rootfs, annotations))

	target, err := os.Readlink(filepath.Join(override.BootByABIDir, abi))
	require.NoError(t, err)
	assert.Equal(t, "../docker/volumes/ext_test_abc_boot/_data/Image", target)
}

// The arm is what opens the validation window, so everything the validator
// reads has to be on disk before it. A failure to arm leaves a link and a
// prestate nobody armed, which the next attempt overwrites.
func TestActivate_ArmComesLast(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	var linkAtArm, prestateAtArm bool
	armOverride = func(string) error {
		_, err := os.Lstat(filepath.Join(override.BootByABIDir, abi))
		linkAtArm = err == nil
		_, err = os.Stat(filepath.Join(override.StateMount, "extension-health-variables"))
		prestateAtArm = err == nil
		return assert.AnError
	}

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, errVerdict, "a failed arm is a machine condition")
	assert.True(t, linkAtArm, "the link must be published before the window opens")
	assert.True(t, prestateAtArm, "the prestate must be on disk before the window opens")
}

// A declined extension leaves no trace: nothing is written before the
// extension is known to be a valid kernel override.
func TestActivate_DeclinedExtensionWritesNothing(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, _ := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")

	var armed int
	armOverride = func(string) error {
		armed++
		return nil
	}

	err := activate(context.Background(), activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	require.ErrorIs(t, err, errVerdict)

	_, statErr := os.Stat(override.BootByABIDir)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(filepath.Join(override.StateMount, "extension-health-variables"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	assert.Zero(t, armed)
}
