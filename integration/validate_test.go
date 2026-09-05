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

const (
	blockPath     = "/mnt/boot/bootenv"
	blockSize     = 1024
	bootByABIDir  = "/mnt/data/boot-by-abi"
	dataMount     = "/mnt/data"
	purgeMarker   = "remove_me_to_reset"
	blockSentinel = "# GRUB Environment Block\n"
)

// seedBlock overwrites the environment block with exactly these entries, in
// the layout grub-editenv leaves: the signature, the entries, then padding to
// a fixed size. grub is not in this image and the manager's own writer is
// what the test exercises.
func seedBlock(t *testing.T, entries ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(blockSentinel)
	for _, e := range entries {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	require.Less(t, b.Len(), blockSize)

	block := make([]byte, blockSize)
	copy(block, b.String())
	for i := b.Len(); i < blockSize; i++ {
		block[i] = '#'
	}
	require.NoError(t, os.WriteFile(blockPath, block, 0o644))
}

// blockEntries reads the block back as a map, dropping the padding.
func blockEntries(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(blockPath)
	require.NoError(t, err)
	require.Len(t, data, blockSize, "a write must not resize the block")

	out := map[string]string{}
	body := strings.TrimPrefix(string(data), blockSentinel)
	for _, line := range strings.Split(body, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || strings.HasPrefix(line, "#") {
			continue
		}
		out[key] = value
	}
	return out
}

// The sweep is the only part of the manager that reads the container store
// off the disk rather than through the engine, so it is the only part a unit
// test has to stub. Here it runs against a real store the engine populated.
//
// This container has no A/B root filesystem, so naming the running slot
// fails and `validate` exits non-zero. That is what makes the assertion
// possible rather than what defeats it: the sweep is deliberately ordered
// above the slot derivation, so a device that cannot name its slot still gets
// its records converged before it refuses.
func TestValidate_SweepsAgainstTheEngineStore(t *testing.T) {
	const kernel = "validate integration kernel bytes"
	sum := sha256.Sum256([]byte(kernel))
	kept := hex.EncodeToString(sum[:])
	// A withdrawn override leaves records nothing claims.
	withdrawn := strings.Repeat("f", len(kept))

	tag := "validate-integration:latest"
	buildExtensionImageWithContent(t, tag, map[string]string{
		"boot/Image": kernel,
	}, "io.balena.image.kernel-abi-id="+kept)

	id := runExtension(t, tag, "validate-integration")
	t.Cleanup(func() {
		dockerExecMayFail(t, "rm", "-f", id)
		_ = os.Remove(filepath.Join(bootByABIDir, kept))
	})

	// Without the marker the claim query cannot tell.
	marker := filepath.Join(dataMount, purgeMarker)
	require.NoError(t, os.WriteFile(marker, nil, 0o644))
	t.Cleanup(func() { _ = os.Remove(marker) })

	link := filepath.Join(bootByABIDir, withdrawn)
	require.NoError(t, os.Symlink("../docker/volumes/gone/_data/Image", link))
	t.Cleanup(func() { _ = os.Remove(link) })

	seedBlock(t,
		"kernel_override_abi="+kept,
		"kernel_override_abi_committed_A="+kept,
		"kernel_override_abi_committed_B="+withdrawn,
	)

	out, err := runManager(t, "validate", "--settle", "0", "--retry", "0", "--attempts", "1")
	require.Error(t, err, "this container has no A/B root filesystem:\n%s", out)
	assert.Contains(t, out, "/mnt/sysroot/active",
		"the refusal must name what could not be read:\n%s", out)

	assert.Equal(t, map[string]string{
		"kernel_override_abi":             kept,
		"kernel_override_abi_committed_A": kept,
	}, blockEntries(t), "only the claimed ABI's records survive")

	_, err = os.Lstat(filepath.Join(bootByABIDir, kept))
	assert.NoError(t, err, "a claimed ABI keeps its published kernel")
	_, err = os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "a withdrawn ABI loses it")
}

// A pending purge wipes the data partition, so the claim set cannot be read
// and every record on it has to be left alone.
func TestValidate_APendingPurgeSweepsNothing(t *testing.T) {
	abi := strings.Repeat("e", 64)

	link := filepath.Join(bootByABIDir, abi)
	require.NoError(t, os.MkdirAll(bootByABIDir, 0o755))
	require.NoError(t, os.Symlink("../docker/volumes/pending/_data/Image", link))
	t.Cleanup(func() { _ = os.Remove(link) })

	// An absent marker is a pending purge.
	_ = os.Remove(filepath.Join(dataMount, purgeMarker))

	seedBlock(t,
		"kernel_override_abi="+abi,
		"kernel_override_abi_committed_A="+abi,
	)

	out, err := runManager(t, "validate", "--settle", "0", "--retry", "0", "--attempts", "1")
	require.Error(t, err, "this container has no A/B root filesystem:\n%s", out)

	assert.Equal(t, map[string]string{
		"kernel_override_abi":             abi,
		"kernel_override_abi_committed_A": abi,
	}, blockEntries(t))
	_, err = os.Lstat(link)
	assert.NoError(t, err, "%s must survive a boot that cannot read the claims", link)
}
