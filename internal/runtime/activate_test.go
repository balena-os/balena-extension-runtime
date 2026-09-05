package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
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

	prevBootByABI, prevState, prevVPN := bootByABIDir, stateMount, vpnActiveMarker
	prevMounted, prevArm := isMounted, armOverride

	bootByABIDir = filepath.Join(root, "mnt", "data", "boot-by-abi")
	stateMount = filepath.Join(root, "mnt", "state")
	vpnActiveMarker = filepath.Join(root, "run", "openvpn", "active")
	isMounted = func(string) (bool, error) { return true, nil }
	armOverride = func(string) error { return nil }

	require.NoError(t, os.MkdirAll(stateMount, 0o755))

	t.Cleanup(func() {
		bootByABIDir, stateMount, vpnActiveMarker = prevBootByABI, prevState, prevVPN
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
// would have, and returns its mountpoint.
func fabricatedVolume(t *testing.T, root, containerID, name string) string {
	t.Helper()
	mountpoint := filepath.Join(root, "var", "lib", "docker", "volumes", name, "_data")
	require.NoError(t, os.MkdirAll(mountpoint, 0o755))
	// create fills the volume from the extension's /boot, so the kernel is
	// in it by the time start publishes a link naming it.
	require.NoError(t, os.WriteFile(filepath.Join(mountpoint, "Image"), nil, 0o644))
	require.NoError(t, oci.WriteBootVolume(containerID, mountpoint))
	return mountpoint
}

// A userspace-only extension is not a kernel override and activation is a
// no-op for it. Nothing about the host is even read.
func TestActivate_NoLabelDoesNothing(t *testing.T) {
	root := activateHost(t)
	rootfs, _ := activateRootfs(t, root, "kernel")

	require.NoError(t, activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class: labels.ClassOverlay,
	}))

	_, err := os.Stat(bootByABIDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "a userspace extension publishes nothing")
}

// The label is the claim and the shipped bytes verify it. Nothing under /boot
// hashing to the label means the image is not what it says it is, and no
// retry changes that.
func TestActivate_NoMatchingKernelIsDeclined(t *testing.T) {
	root := activateHost(t)
	rootfs, _ := activateRootfs(t, root, "kernel")

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	assert.ErrorIs(t, err, hooks.ErrRejected)
}

// The bytes verified have to live inside the extension being verified.
func TestActivate_KernelOnlyAsASymlinkIsDeclined(t *testing.T) {
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	real := filepath.Join(rootfs, "boot", "Image")
	elsewhere := filepath.Join(root, "elsewhere")
	require.NoError(t, os.Rename(real, elsewhere))
	require.NoError(t, os.Symlink(elsewhere, real))

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	assert.ErrorIs(t, err, hooks.ErrRejected)
}

func TestActivate_AbsentBootIsDeclined(t *testing.T) {
	root := activateHost(t)
	rootfs := filepath.Join(root, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	assert.ErrorIs(t, err, hooks.ErrRejected)
}

// A /boot that cannot be read says nothing about the image, so it must not be
// recorded as the extension refusing. Only its absence is the image's fault.
func TestActivate_UnreadableBootIsRetryable(t *testing.T) {
	root := activateHost(t)
	rootfs := filepath.Join(root, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootfs, "boot"), nil, 0o644))

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, hooks.ErrRejected)
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

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	assert.ErrorIs(t, err, hooks.ErrRejected)
	assert.Zero(t, armed)
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

	require.NoError(t, activate(activateTestLogger, "c1", rootfs, map[string]string{
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
	mountpoint := fabricatedVolume(t, root, "c1", "ext_test_abc_boot")
	require.NoError(t, os.Remove(filepath.Join(mountpoint, "Image")))

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, hooks.ErrRejected)
}

// A kernel the boot-time validator rejected must not go straight back. The
// record holds one bare ABI per line and the ABI is a hash of the image, so
// the entry stops applying as soon as the extension ships different bytes.
func TestActivate_RejectedABIIsDeclined(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "c1", "ext_test_abc_boot")
	require.NoError(t, os.WriteFile(filepath.Join(stateMount, "override-rejected"),
		[]byte("1111111111111111111111111111111111111111111111111111111111111111\n"+abi+"\n"), 0o644))

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	assert.ErrorIs(t, err, hooks.ErrRejected)
}

// A boot that races the state mount is a machine condition, not the extension
// declining. Recording it as a decline marks the extension as having refused,
// permanently, for something a later boot fixes.
func TestActivate_UnmountedStateIsRetryable(t *testing.T) {
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	isMounted = func(string) (bool, error) { return false, nil }

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, hooks.ErrRejected)
}

// An unreadable rejection record cannot tell "listed" from "not listed", so
// it is a machine condition rather than a verdict.
func TestActivate_UnreadableRejectionRecordIsRetryable(t *testing.T) {
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	require.NoError(t, os.Mkdir(filepath.Join(stateMount, "override-rejected"), 0o755))

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, hooks.ErrRejected)
}

func TestActivate_MissingVolumeRecordIsRetryable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, hooks.ErrRejected)
}

// The engine's data root is a bind of the data partition's, so a mountpoint
// that is not where the data partition expects it means the engine's layout
// is not the one this OS was built against.
func TestActivate_ForeignVolumeLayoutIsRetryable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	stray := filepath.Join(root, "somewhere", "else")
	require.NoError(t, os.MkdirAll(stray, 0o755))
	require.NoError(t, oci.WriteBootVolume("c1", stray))

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, hooks.ErrRejected)
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

	require.NoError(t, activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}))

	// The link is relative to the data partition, not to the engine's data
	// root, so it resolves the same way in the initramfs.
	target, err := os.Readlink(filepath.Join(bootByABIDir, abi))
	require.NoError(t, err)
	assert.Equal(t, "../docker/volumes/ext_test_abc_boot/_data/Image", target)

	prestate, err := os.ReadFile(filepath.Join(stateMount, "extension-health-variables"))
	require.NoError(t, err)
	assert.Equal(t, "BALENAOS_ROLLBACK_VPNONLINE=0\n", string(prestate))

	assert.Equal(t, []string{abi}, armed)

	entries, err := os.ReadDir(bootByABIDir)
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

	path := filepath.Join(stateMount, "extension-health-variables")
	require.NoError(t, os.WriteFile(path, []byte("BALENAOS_ROLLBACK_VPNONLINE=1\n"), 0o644))

	// The rename is the only thing that may touch the published name.
	armOverride = func(string) error {
		entries, err := os.ReadDir(stateMount)
		require.NoError(t, err)
		for _, e := range entries {
			assert.NotContains(t, e.Name(), ".new", "the temporary name must not survive")
		}
		return nil
	}

	require.NoError(t, activate(activateTestLogger, "c1", rootfs, map[string]string{
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
	require.NoError(t, os.MkdirAll(filepath.Dir(vpnActiveMarker), 0o755))
	require.NoError(t, os.WriteFile(vpnActiveMarker, nil, 0o644))

	require.NoError(t, activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}))

	prestate, err := os.ReadFile(filepath.Join(stateMount, "extension-health-variables"))
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
	require.NoError(t, activate(activateTestLogger, "c1", rootfs, annotations))
	require.NoError(t, activate(activateTestLogger, "c1", rootfs, annotations))

	target, err := os.Readlink(filepath.Join(bootByABIDir, abi))
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
		_, err := os.Lstat(filepath.Join(bootByABIDir, abi))
		linkAtArm = err == nil
		_, err = os.Stat(filepath.Join(stateMount, "extension-health-variables"))
		prestateAtArm = err == nil
		return assert.AnError
	}

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, hooks.ErrRejected, "a failed arm is a machine condition")
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

	err := activate(activateTestLogger, "c1", rootfs, map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	require.ErrorIs(t, err, hooks.ErrRejected)

	_, statErr := os.Stat(bootByABIDir)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(filepath.Join(stateMount, "extension-health-variables"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
	assert.Zero(t, armed)
}

func TestVolumeTarget(t *testing.T) {
	target, err := volumeTarget("/var/lib/docker/volumes/ext_svc_abc_boot/_data", "Image")
	require.NoError(t, err)
	assert.Equal(t, "../docker/volumes/ext_svc_abc_boot/_data/Image", target)

	for _, bad := range []string{
		"/var/lib/docker/volumes/ext_svc_abc_boot",
		"/var/lib/docker/ext_svc_abc_boot/_data",
		"/_data",
		"",
	} {
		_, err := volumeTarget(bad, "Image")
		assert.Error(t, err, "%q is not a fabricated volume mountpoint", bad)
	}

	// The link names one file inside the volume, so anything carrying a
	// separator would reach outside it.
	for _, bad := range []string{"", "sub/Image", "../Image"} {
		_, err := volumeTarget("/var/lib/docker/volumes/ext_svc_abc_boot/_data", bad)
		assert.Error(t, err, "%q is not a bare kernel image name", bad)
	}
}
