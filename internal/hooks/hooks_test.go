package hooks

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

func TestExecuteIfPresentMissing(t *testing.T) {
	rootfs := t.TempDir()
	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "hooks/create", map[string]string{}, nil)
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

	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "hooks/create", annotations, nil)
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
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "sha256:abc123",
	}

	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "hooks/start", annotations, nil)
	require.NoError(t, err)

	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "EXTENSION_ROOTFS="+rootfs)
	assert.Contains(t, content, "EXTENSION_IMAGE_CLASS=overlay")
	assert.Contains(t, content, "EXTENSION_IMAGE_KERNEL_ABI_ID=sha256:abc123")
}

// TestExecuteIfPresentSanitizesEnv asserts that the runtime's process env
// does NOT leak into hook subprocesses. Inherited vars like containerd auth
// tokens or TTRPC addresses would be a privileged-runtime privacy leak.
// Hooks must see only PATH, EXTENSION_ROOTFS, the extension label env and the
// mount volume env.
func TestExecuteIfPresentSanitizesEnv(t *testing.T) {
	// Set a sentinel var in the test's (parent) process env. If the hook
	// inherits, it'll dump this via `env` into the output file.
	t.Setenv("BALENA_RUNTIME_SECRET_SENTINEL", "must-not-leak-to-hook")

	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	envFile := filepath.Join(t.TempDir(), "env")
	hookScript := "#!/bin/sh\nenv > " + envFile + "\n"
	scriptPath := filepath.Join(hookDir, "create")
	require.NoError(t, os.WriteFile(scriptPath, []byte(hookScript), 0o755))

	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "hooks/create",
		map[string]string{"io.balena.image.class": "overlay"}, nil)
	require.NoError(t, err)

	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	content := string(data)

	assert.NotContains(t, content, "BALENA_RUNTIME_SECRET_SENTINEL",
		"hook must not inherit parent process env")
	assert.Contains(t, content, "PATH=/usr/sbin:/usr/bin:/sbin:/bin")
	assert.Contains(t, content, "EXTENSION_ROOTFS="+rootfs)
	assert.Contains(t, content, "EXTENSION_IMAGE_CLASS=overlay")
}

func TestExecuteIfPresentFailure(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	hookScript := "#!/bin/sh\nexit 1\n"
	hookPath := filepath.Join(hookDir, "create")
	require.NoError(t, os.WriteFile(hookPath, []byte(hookScript), 0o755))

	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "hooks/create", map[string]string{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hook")
}

// TestExecuteIfPresentHonoursCallerCancellation pins the property a timed-out
// lifecycle call depends on. containerd bounds the call and retries it, and the
// retry runs the same hook again; a hook that kept running past the caller's
// cancellation would be publishing the same kernel the retry's hook is writing.
func TestExecuteIfPresentHonoursCallerCancellation(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	// exec, so the process CommandContext kills is the sleep itself. A plain
	// `sleep` would be a grandchild, outliving the kill and holding the test's
	// stderr open until it finished on its own.
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "create"),
		[]byte("#!/bin/sh\nexec sleep 30\n"), 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := ExecuteIfPresent(ctx, testLogger, rootfs, "hooks/create", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aborted",
		"a cancelled caller is not a misbehaving hook, and the message must say so")
	assert.Less(t, time.Since(start), 10*time.Second,
		"cancellation must kill the hook rather than wait out hookTimeout")
}

func TestExecuteIfPresentRejectsTraversal(t *testing.T) {
	rootfs := t.TempDir()
	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "../../etc/passwd", map[string]string{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes rootfs")
}

func TestExecuteIfPresentRejectsAbsolute(t *testing.T) {
	rootfs := t.TempDir()
	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "/etc/passwd", map[string]string{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be relative")
}

func TestExecuteIfPresentDirectory(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks", "create")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "hooks/create", map[string]string{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")
}

func TestExecuteIfPresent_ExportsMountVolumeEnv(t *testing.T) {
	rootfs := t.TempDir()
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	envFile := filepath.Join(t.TempDir(), "env")
	hookScript := "#!/bin/sh\nenv | grep ^EXTENSION_ > " + envFile + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "create"), []byte(hookScript), 0o755))

	specMounts := []specs.Mount{
		{Destination: "/boot", Source: "/mnt/data/docker/volumes/v1/_data"},
	}

	err := ExecuteIfPresent(context.Background(), testLogger, rootfs, "hooks/create",
		map[string]string{"io.balena.image.class": "overlay"},
		specMounts,
	)
	require.NoError(t, err)

	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), "EXTENSION_VOLUME_BOOT=/mnt/data/docker/volumes/v1/_data")
}
