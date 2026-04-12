package hooks

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

func TestExecuteIfPresentMissing(t *testing.T) {
	rootfs := t.TempDir()
	err := ExecuteIfPresent(testLogger, rootfs, "hooks/create", map[string]string{})
	require.NoError(t, err)
}

func TestExecuteIfPresentSuccess(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	// Write a hook that creates a marker file
	marker := filepath.Join(t.TempDir(), "marker")
	hookScript := "#!/bin/sh\ntouch " + marker + "\n"
	hookPath := filepath.Join(hookDir, "create")
	require.NoError(t, os.WriteFile(hookPath, []byte(hookScript), 0o755))

	annotations := map[string]string{
		"io.balena.image.class": "overlay",
	}

	err := ExecuteIfPresent(testLogger, rootfs, "hooks/create", annotations)
	require.NoError(t, err)

	_, err = os.Stat(marker)
	require.NoError(t, err, "hook should have created marker file")
}

func TestExecuteIfPresentEnvVars(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	// Hook that writes env vars to a file
	envFile := filepath.Join(t.TempDir(), "env")
	hookScript := "#!/bin/sh\nenv | grep EXTENSION_ > " + envFile + "\n"
	hookPath := filepath.Join(hookDir, "start")
	require.NoError(t, os.WriteFile(hookPath, []byte(hookScript), 0o755))

	annotations := map[string]string{
		"io.balena.image.class":          "overlay",
		"io.balena.image.kernel-abi-id":  "sha256:abc123",
	}

	err := ExecuteIfPresent(testLogger, rootfs, "hooks/start", annotations)
	require.NoError(t, err)

	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "EXTENSION_ROOTFS="+rootfs)
	assert.Contains(t, content, "EXTENSION_IMAGE_CLASS=overlay")
	assert.Contains(t, content, "EXTENSION_IMAGE_KERNEL_ABI_ID=sha256:abc123")
}

func TestExecuteIfPresentFailure(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	hookScript := "#!/bin/sh\nexit 1\n"
	hookPath := filepath.Join(hookDir, "create")
	require.NoError(t, os.WriteFile(hookPath, []byte(hookScript), 0o755))

	err := ExecuteIfPresent(testLogger, rootfs, "hooks/create", map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hook")
}

func TestExecuteIfPresentDirectory(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks", "create")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	err := ExecuteIfPresent(testLogger, rootfs, "hooks/create", map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")
}
