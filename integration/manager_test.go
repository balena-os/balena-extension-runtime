package integration_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Cleanup tests ---

// Plain `cleanup` must preserve stale-kernel containers (they are the
// rollback target outside the HUP post-commit window).
func TestCleanup_DeadOnly_KeepsStaleKernelContainers(t *testing.T) {
	tag := uniqueName("ext-stale-plain")
	buildExtensionImage(t, tag)

	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"--label", "io.balena.image.kernel-version=99.99.99",
		tag, "none")
	defer dockerExecMayFail(t, "rm", "-f", id)

	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out, "plain cleanup must preserve stale-kernel container")
}

func TestCleanup_StaleOS_RemovesStaleKernelContainers(t *testing.T) {
	tag := uniqueName("ext-stale")
	buildExtensionImage(t, tag)

	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"--label", "io.balena.image.kernel-version=99.99.99",
		tag, "none")

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	require.NotEmpty(t, out, "container should exist before cleanup")

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out = dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.Empty(t, out, "stale-kernel container should be removed under --stale-os")
}

func TestCleanup_StaleOS_KeepsMatchingKernelContainers(t *testing.T) {
	tag := uniqueName("ext-keep")
	kver := hostKernelVersion(t)
	buildExtensionImage(t, tag)

	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"--label", "io.balena.image.kernel-version="+kver,
		tag, "none")
	defer dockerExecMayFail(t, "rm", "-f", id)

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out, "container with matching kernel should survive --stale-os cleanup")
}

// Plain `cleanup` must not touch images regardless of label.
func TestCleanup_DeadOnly_KeepsImages(t *testing.T) {
	tag := uniqueName("ext-plain-image")
	buildExtensionImage(t, tag, "io.balena.image.os-version=99.99.99")

	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	imageID := dockerExec(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	assert.NotEmpty(t, imageID, "plain cleanup must not remove images")

	dockerExec(t, "rmi", "-f", tag)
}

func TestCleanup_StaleOS_RemovesOSStaleImages(t *testing.T) {
	tag := uniqueName("ext-osstale")
	buildExtensionImage(t, tag, "io.balena.image.os-version=99.99.99")

	imageID := dockerExec(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	require.NotEmpty(t, imageID, "image should exist")

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out, _ := dockerExecMayFail(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	assert.Empty(t, out, "os-stale extension image should be removed under --stale-os")
}

func TestCleanup_StaleOS_KeepsMatchingOSImages(t *testing.T) {
	tag := uniqueName("ext-osmatch")
	osVer := hostOSVersion(t)
	buildExtensionImage(t, tag, "io.balena.image.os-version="+osVer)
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	imageID := dockerExec(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	assert.NotEmpty(t, imageID, "image with matching os-version should survive --stale-os")
}

// An extension image with no os-version label is legacy and must be retained.
func TestCleanup_StaleOS_KeepsLegacyImages(t *testing.T) {
	tag := uniqueName("ext-legacy")
	buildExtensionImage(t, tag)
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	imageID := dockerExec(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	assert.NotEmpty(t, imageID, "legacy image without os-version label must be retained")
}

// Symmetric to KeepsLegacyImages: a container carrying only the class label
// (no kernel-version, kernel-abi-id, or os-version) must survive --stale-os.
func TestCleanup_StaleOS_KeepsLegacyContainers(t *testing.T) {
	tag := uniqueName("ext-legacy-ct")
	buildExtensionImage(t, tag)
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		tag, "none")
	defer dockerExecMayFail(t, "rm", "-f", id)

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out, "legacy container without compat labels must be retained")
}

// Safety invariant: cleanup acts only on io.balena.image.class=overlay
// containers. A non-extension container with an otherwise-stale
// kernel-version label must be untouched under --stale-os.
func TestCleanup_StaleOS_IgnoresNonExtensionContainers(t *testing.T) {
	tag := uniqueName("plain-img")
	// Build the image without the class=overlay label so the container
	// inherits nothing extension-related except what we attach below.
	cmd := []string{"import", "-", tag}
	runDocker := exec.Command("docker", cmd...)
	runDocker.Stdin = bytes.NewReader(make([]byte, 1024))
	require.NoError(t, runDocker.Run(), "docker import failed")
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	id := dockerExec(t, "create",
		"--label", "io.balena.image.kernel-version=99.99.99",
		tag, "none")
	defer dockerExecMayFail(t, "rm", "-f", id)

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out, "non-extension container must not be touched by cleanup")
}

// Exercises the os-version label as a glob: a pattern like "<major>.*" that
// matches the host VERSION_ID should retain the container end-to-end.
func TestCleanup_StaleOS_OSVersionGlobMatches(t *testing.T) {
	osVer := hostOSVersion(t)
	major, _, _ := strings.Cut(osVer, ".")
	if major == osVer || major == "" {
		t.Skipf("host VERSION_ID %q has no dotted form; glob test not applicable", osVer)
	}
	glob := major + ".*"

	tag := uniqueName("ext-osglob")
	buildExtensionImage(t, tag, "io.balena.image.os-version="+glob)
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"--label", "io.balena.image.os-version="+glob,
		tag, "none")
	defer dockerExecMayFail(t, "rm", "-f", id)

	mgrOut, err := runManager(t, "cleanup", "--stale-os")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out, "container whose os-version glob matches host must be retained")

	imageID := dockerExec(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	assert.NotEmpty(t, imageID, "image whose os-version glob matches host must be retained")
}

// TestCleanup_Zombie_RemovesFailedStart provokes a real failed-Start
// zombie by starting a container whose entrypoint binary does not exist
// in the image,
func TestCleanup_Zombie_RemovesFailedStart(t *testing.T) {
	tag := uniqueName("ext-zombie-fs")
	buildExtensionImage(t, tag)
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		tag, "no-such-binary")
	defer dockerExecMayFail(t, "rm", "-f", id)

	// Start must fail — empty rootfs cannot exec anything. A successful
	// start means the engine has changed in a way that invalidates the
	// fixture; fail loudly rather than skip.
	_, startErr := dockerExecMayFail(t, "start", id)
	require.Error(t, startErr,
		"docker start must fail against an empty-rootfs image; "+
			"engine behaviour has drifted and the zombie fixture is broken")

	state := dockerExec(t, "inspect", "--format", "{{.State.Status}}", id)
	stateErr := dockerExec(t, "inspect", "--format", "{{.State.Error}}", id)
	require.NotEmpty(t, stateErr,
		"engine did not populate State.Error for a failed-Start container "+
			"(state=%q); the zombie-sweep predicate assumes this field is "+
			"the daemon-authoritative signal", state)

	t.Logf("zombie precondition: state=%q State.Error=%q", state, stateErr)

	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out, _ := dockerExecMayFail(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.Empty(t, out,
		"zombie container with State.Error set must be removed by cleanup; "+
			"state at start was %q, State.Error was %q", state, stateErr)
}

// TestCleanup_Zombie_PreservesCleanExited pins the negative invariant
// against a real engine.
func TestCleanup_Zombie_PreservesCleanExited(t *testing.T) {
	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"busybox", "true")
	defer dockerExecMayFail(t, "rm", "-f", id)

	_, err := dockerExecMayFail(t, "start", "-a", id)
	require.NoError(t, err, "clean-exit fixture must start successfully")

	state := dockerExec(t, "inspect", "--format", "{{.State.Status}}", id)
	stateErr := dockerExec(t, "inspect", "--format", "{{.State.Error}}", id)
	require.Equal(t, "exited", state, "fixture must be in exited state")
	require.Empty(t, strings.TrimSpace(stateErr),
		"fixture must have empty State.Error; got %q", stateErr)

	mgrOut, mgrErr := runManager(t, "cleanup")
	require.NoError(t, mgrErr, "cleanup failed: %s", mgrOut)

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out,
		"clean-exited extension container with no State.Error must be preserved")
}

// TestCleanup_Zombie_IgnoresNonExtensionContainers pins the class filter
// against a real engine: a non-extension container with State.Error
// populated must not be removed.
func TestCleanup_Zombie_IgnoresNonExtensionContainers(t *testing.T) {
	tag := uniqueName("plain-zombie")
	// No overlay class label → container is outside cleanup's scope
	// regardless of any other label it carries.
	buildExtensionImageNoClass(t, tag)
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	id := dockerExec(t, "create", tag, "no-such-binary")
	defer dockerExecMayFail(t, "rm", "-f", id)

	_, startErr := dockerExecMayFail(t, "start", id)
	require.Error(t, startErr,
		"docker start must fail against an empty-rootfs image; "+
			"engine behaviour has drifted and the fixture is broken")
	stateErr := dockerExec(t, "inspect", "--format", "{{.State.Error}}", id)
	require.NotEmpty(t, stateErr, "non-extension fixture must have State.Error populated")

	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out,
		"non-extension zombie must be ignored by cleanup (class label filter)")
}
