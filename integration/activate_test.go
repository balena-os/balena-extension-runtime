package integration_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A kernel override deployed through the engine publishes its kernel and arms
// it. The link is asserted by resolving it, which is the only check that the
// target is relative to the data partition rather than to the engine's own
// data root.
func TestActivate_PublishesAndArmsThroughTheEngine(t *testing.T) {
	const kernel = "integration kernel bytes"
	sum := sha256.Sum256([]byte(kernel))
	abi := hex.EncodeToString(sum[:])

	tag := "activate-integration:latest"
	buildExtensionImageWithContent(t, tag, map[string]string{
		"boot/Image": kernel,
	}, "io.balena.image.kernel-abi-id="+abi)

	id := runExtension(t, tag, "activate-integration")
	t.Cleanup(func() {
		dockerExec(t, "rm", "-f", id)
		_ = os.Remove(filepath.Join("/mnt/data/boot-by-abi", abi))
	})

	link := filepath.Join("/mnt/data/boot-by-abi", abi)
	target, err := os.Readlink(link)
	require.NoError(t, err, "the extension must publish its kernel")
	assert.True(t, strings.HasPrefix(target, "../docker/volumes/"),
		"the link must be relative to the data partition, got %q", target)
	assert.True(t, strings.HasSuffix(target, "/Image"),
		"the link must name the kernel image, got %q", target)

	// Reading through it is what proves the target is right: the initramfs
	// reads the link under its own data mount, not under /var/lib/docker.
	published, err := os.ReadFile(link)
	require.NoError(t, err, "the published link must resolve to the kernel image")
	assert.Equal(t, kernel, string(published))

	block, err := os.ReadFile("/mnt/boot/bootenv")
	require.NoError(t, err)
	require.Len(t, block, 1024)
	entries := string(block)
	assert.Contains(t, entries, "kernel_override_abi="+abi+"\n", "the override must be armed")
	assert.NotContains(t, entries, "kernel_override_trial",
		"a new window starts from zero")

	prestate, err := os.ReadFile("/mnt/state/extension-health-variables")
	require.NoError(t, err)
	assert.Contains(t, string(prestate), "BALENAOS_ROLLBACK_VPNONLINE=")
}

// A userspace-only extension activates nothing, on the same engine.
func TestActivate_UserspaceExtensionPublishesNothing(t *testing.T) {
	tag := "activate-userspace:latest"
	buildExtensionImageWithContent(t, tag, map[string]string{
		"usr/lib/thing.so": "content",
	})

	id := runExtension(t, tag, "activate-userspace")
	t.Cleanup(func() { dockerExec(t, "rm", "-f", id) })

	entries, err := os.ReadDir("/mnt/data/boot-by-abi")
	if err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)
		return
	}
	for _, e := range entries {
		assert.NotContains(t, e.Name(), "activate-userspace")
	}
}
