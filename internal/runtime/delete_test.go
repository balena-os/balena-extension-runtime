package runtime

import (
	"encoding/json"
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

// validBundle writes a minimal OCI config.json pointing at an existing
// rootfs directory, so ReadSpec + ResolveRootfs succeed.
func validBundle(t *testing.T) string {
	t.Helper()
	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))

	spec := specs.Spec{
		Version: specs.Version,
		Root:    &specs.Root{Path: "rootfs"},
	}
	data, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644))
	return bundle
}

// TestForceDelete_MissingState asserts force-delete is idempotent: when
// state has already been reaped, Delete returns nil rather than erroring.
func TestForceDelete_MissingState(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	err := Delete(testLogger(), "never-existed", true)
	assert.NoError(t, err)
}

// TestForceDelete_HookFailureReturnsError asserts that errors from
// runDeleteHook propagate through the deferred joined error — the previous
// behaviour was to log and swallow. State removal still runs regardless.
func TestForceDelete_HookFailureReturnsError(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	// Bundle has no config.json — runDeleteHook's ReadSpec fails.
	bundle := t.TempDir()

	const id = "hook-fail-test"
	state := oci.NewState(id, bundle)
	state.Status = specs.StateStopped
	require.NoError(t, oci.WriteState(state))

	err := Delete(testLogger(), id, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete hook")

	// State should still have been removed on the way out.
	_, readErr := oci.ReadState(id)
	require.Error(t, readErr)
	assert.True(t, errors.Is(readErr, os.ErrNotExist), "state must be removed even when hook fails")
}

// TestForceDelete_Success asserts the happy path returns nil and removes state.
func TestForceDelete_Success(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	bundle := validBundle(t)

	const id = "happy-force-delete"
	state := oci.NewState(id, bundle)
	state.Status = specs.StateStopped
	require.NoError(t, oci.WriteState(state))

	err := Delete(testLogger(), id, true)
	assert.NoError(t, err)

	_, readErr := oci.ReadState(id)
	assert.True(t, errors.Is(readErr, os.ErrNotExist))
}
