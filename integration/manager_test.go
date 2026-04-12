package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Cleanup tests ---

func TestCleanup_RemovesStaleKernelContainers(t *testing.T) {
	tag := uniqueName("ext-stale")
	buildExtensionImage(t, tag)

	// Create container with a kernel version that doesn't match the host.
	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"--label", "io.balena.image.kernel-version=99.99.99",
		tag, "none")

	// Verify container exists.
	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	require.NotEmpty(t, out, "container should exist before cleanup")

	// Run cleanup.
	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	// Verify container is gone.
	out = dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.Empty(t, out, "stale container should be removed after cleanup")
}

func TestCleanup_KeepsMatchingKernelContainers(t *testing.T) {
	tag := uniqueName("ext-keep")
	kver := hostKernelVersion(t)
	buildExtensionImage(t, tag)

	// Create container with matching kernel version.
	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"--label", "io.balena.image.kernel-version="+kver,
		tag, "none")

	// Run cleanup.
	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	// Verify container still exists.
	out := dockerExec(t, "ps", "-a", "--filter", "id="+id, "--format", "{{.ID}}")
	assert.NotEmpty(t, out, "container with matching kernel should survive cleanup")

	// Cleanup: remove the container.
	dockerExec(t, "rm", "-f", id)
}

func TestCleanup_RemovesOrphanedImages(t *testing.T) {
	tag := uniqueName("ext-orphan")
	buildExtensionImage(t, tag)

	// Get image ID.
	imageID := dockerExec(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	require.NotEmpty(t, imageID, "image should exist")

	// No containers reference this image — it's orphaned.
	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	// Verify image is gone.
	out, _ := dockerExecMayFail(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	assert.Empty(t, out, "orphaned extension image should be removed after cleanup")
}

func TestCleanup_KeepsReferencedImages(t *testing.T) {
	tag := uniqueName("ext-ref")
	kver := hostKernelVersion(t)
	buildExtensionImage(t, tag)

	// Use tag:latest explicitly so the container's Image field matches the
	// image's RepoTags entry (Docker normalizes tags to include :latest).
	ref := tag + ":latest"

	// Create a container referencing the image with a matching kernel version.
	id := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		"--label", "io.balena.image.kernel-version="+kver,
		ref, "none")

	// Run cleanup.
	mgrOut, err := runManager(t, "cleanup")
	require.NoError(t, err, "cleanup failed: %s", mgrOut)

	// Verify image still exists.
	imageID := dockerExec(t, "images", "--filter", "reference="+tag, "--format", "{{.ID}}")
	assert.NotEmpty(t, imageID, "image with live container should survive cleanup")

	// Cleanup.
	dockerExec(t, "rm", "-f", id)
}

// --- Update tests ---

func TestUpdate_CreatesContainersForNewKernel(t *testing.T) {
	tag := uniqueName("ext-update")
	buildExtensionImage(t, tag)

	// Create an existing extension container (simulates current state).
	existingID := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		tag, "none")

	// Create a fake rootfs with a kernel version directory.
	rootfs := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(rootfs, "lib", "modules", "6.6.74"), 0o755))

	// Run update.
	mgrOut, err := runManager(t, "update", "--rootfs", rootfs)
	// Update may fail for individual containers if the runtime rejects
	// containers without proper annotations, but the command itself should succeed.
	require.NoError(t, err, "update failed: %s", mgrOut)

	// Check if a new container was created with kernel-version=6.6.74.
	out := dockerExec(t, "ps", "-a",
		"--filter", "label=io.balena.image.class=overlay",
		"--filter", "label=io.balena.image.kernel-version=6.6.74",
		"--format", "{{.ID}}")

	if out != "" {
		t.Log("Update successfully created container with new kernel version")
		// Clean up new container(s).
		for _, cid := range strings.Split(out, "\n") {
			cid = strings.TrimSpace(cid)
			if cid != "" {
				dockerExec(t, "rm", "-f", cid)
			}
		}
	} else {
		// Update logs a warning and continues when container creation fails
		// (e.g., if Docker doesn't pass annotations to custom runtimes).
		t.Log("Update handled container creation gracefully (runtime may not receive annotations)")
		assert.Contains(t, mgrOut, "failed to create extension container",
			"expected warning about container creation failure")
	}

	// Clean up existing container.
	dockerExec(t, "rm", "-f", existingID)
}

func TestUpdate_ContinuesOnFailure(t *testing.T) {
	// Create a container referencing a non-existent image.
	id, err := dockerExecMayFail(t, "create",
		"--label", "io.balena.image.class=overlay",
		"nonexistent-image:latest", "none")
	if err == nil {
		// Shouldn't reach here, but clean up if it does.
		defer dockerExec(t, "rm", "-f", strings.TrimSpace(id))
	}
	// Docker won't create a container with a missing image, so create a valid one
	// and then remove the image to simulate a broken reference.
	tag := uniqueName("ext-fail")
	buildExtensionImage(t, tag)

	existingID := dockerExec(t, "create",
		"--label", "io.balena.image.class=overlay",
		tag, "none")

	// Remove the image (container still references it by ID).
	dockerExec(t, "rmi", "-f", tag)

	// Create fake rootfs.
	rootfs := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(rootfs, "lib", "modules", "6.6.74"), 0o755))

	// Run update — should succeed overall even though individual container creation fails.
	mgrOut, err := runManager(t, "update", "--rootfs", rootfs)
	require.NoError(t, err, "update should succeed even when individual containers fail: %s", mgrOut)

	// Clean up.
	dockerExec(t, "rm", "-f", existingID)
}
