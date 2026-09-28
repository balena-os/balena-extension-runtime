package runtime

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestForceDelete_MissingState asserts force-delete is idempotent: when
// state has already been reaped, Delete returns nil rather than erroring.
func TestForceDelete_MissingState(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	err := Delete(testLogger(), "never-existed", true)
	assert.NoError(t, err)
}

// TestForceDelete_NoConfigJSON_Succeeds asserts the invariant that
// force-delete never reads the bundle: a bundle with no config.json is not
// an error.
func TestForceDelete_NoConfigJSON_Succeeds(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	bundle := t.TempDir()

	const id = "no-config-json-test"
	state := oci.NewState(id, bundle)
	state.Status = specs.StateStopped
	require.NoError(t, oci.WriteState(state))

	err := Delete(testLogger(), id, true)
	assert.NoError(t, err)

	_, readErr := oci.ReadState(id)
	require.Error(t, readErr)
	assert.True(t, errors.Is(readErr, os.ErrNotExist), "state must be removed")
}

// TestSoftDelete_BundleGone_RemovesState asserts soft-delete never reads the
// bundle: a stopped container whose bundle directory does not exist still
// has its state cleaned up.
func TestSoftDelete_BundleGone_RemovesState(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	bundle := filepath.Join(t.TempDir(), "gone")
	const id = "bundle-gone-test"
	state := oci.NewState(id, bundle)
	state.Status = specs.StateStopped
	require.NoError(t, oci.WriteState(state))

	err := Delete(testLogger(), id, false)
	assert.NoError(t, err)

	_, readErr := oci.ReadState(id)
	assert.True(t, errors.Is(readErr, os.ErrNotExist))
}
