package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var runtimeBin string

func TestMain(m *testing.M) {
	// Build the runtime binary for e2e tests
	bin, err := filepath.Abs("../balena-extension-runtime")
	if err != nil {
		panic(err)
	}
	runtimeBin = bin

	if _, err := os.Stat(runtimeBin); os.IsNotExist(err) {
		panic("runtime binary not found at " + runtimeBin + " — run 'make build' first")
	}

	os.Exit(m.Run())
}

func setupBundle(t *testing.T, annotations map[string]string) string {
	t.Helper()
	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))

	spec := specs.Spec{
		Version: specs.Version,
		Root:    &specs.Root{Path: "rootfs", Readonly: true},
		Process: &specs.Process{
			Args: []string{"none"},
		},
		Annotations: annotations,
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644))

	return bundle
}

func runRuntime(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(runtimeBin, args...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+t.TempDir())
	return cmd.CombinedOutput()
}

func TestCreateStartLifecycle(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", stateDir)

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class": "overlay",
	})

	containerID := "test-ext-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	pidFile := filepath.Join(t.TempDir(), "pid")

	// Create
	cmd := exec.Command(runtimeBin, "create", "--bundle", bundle, "--pid-file", pidFile, containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "create failed: %s", string(out))

	// Verify PID file
	pidData, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(pidData))
	require.NoError(t, err)
	assert.Greater(t, pid, 0)

	// Verify state
	cmd = exec.Command(runtimeBin, "state", containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "state failed: %s", string(out))

	var state specs.State
	require.NoError(t, json.Unmarshal(out, &state))
	assert.Equal(t, specs.StateCreated, state.Status)
	assert.Equal(t, pid, state.Pid)

	// Start — proxy exits, container becomes stopped
	cmd = exec.Command(runtimeBin, "start", containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "start failed: %s", string(out))

	// Wait for proxy to actually exit
	time.Sleep(100 * time.Millisecond)

	// Verify stopped state
	cmd = exec.Command(runtimeBin, "state", containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "state failed: %s", string(out))

	require.NoError(t, json.Unmarshal(out, &state))
	assert.Equal(t, specs.StateStopped, state.Status)

	// Delete
	cmd = exec.Command(runtimeBin, "delete", containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "delete failed: %s", string(out))

	// Verify gone
	cmd = exec.Command(runtimeBin, "state", containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err = cmd.CombinedOutput()
	require.Error(t, err, "state should fail after delete")
}

func TestCreateRejectsInvalidLabels(t *testing.T) {
	stateDir := t.TempDir()

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class": "volume",
	})

	cmd := exec.Command(runtimeBin, "create", "--bundle", bundle, "bad-label-test")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "unsupported")
}

func TestCreateRejectsMissingLabels(t *testing.T) {
	stateDir := t.TempDir()

	bundle := setupBundle(t, map[string]string{})

	cmd := exec.Command(runtimeBin, "create", "--bundle", bundle, "no-label-test")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "missing required label")
}

func TestKillProxy(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", stateDir)

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class": "overlay",
	})

	containerID := "kill-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	pidFile := filepath.Join(t.TempDir(), "pid")

	// Create
	cmd := exec.Command(runtimeBin, "create", "--bundle", bundle, "--pid-file", pidFile, containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "create failed: %s", string(out))

	// Read PID
	pidData, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(pidData))
	require.NoError(t, err)

	// Verify proxy is alive
	require.NoError(t, syscall.Kill(pid, 0), "proxy should be alive")

	// Kill
	cmd = exec.Command(runtimeBin, "kill", containerID, "SIGTERM")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err = cmd.CombinedOutput()
	require.NoError(t, err, "kill failed: %s", string(out))

	// Wait for process to die
	time.Sleep(100 * time.Millisecond)

	// Verify proxy is dead
	err = syscall.Kill(pid, 0)
	assert.Error(t, err, "proxy should be dead after kill")

	// Force delete
	cmd = exec.Command(runtimeBin, "delete", "--force", containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	_, _ = cmd.CombinedOutput()
}

func TestHookExecution(t *testing.T) {
	stateDir := t.TempDir()

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "sha256:abc123",
	})

	// Add a create hook
	hookDir := filepath.Join(bundle, "rootfs", "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))

	marker := filepath.Join(t.TempDir(), "hook-ran")
	hookScript := "#!/bin/sh\necho \"rootfs=$EXTENSION_ROOTFS\" > " + marker + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "create"), []byte(hookScript), 0o755))

	containerID := "hook-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	cmd := exec.Command(runtimeBin, "create", "--bundle", bundle, containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "create failed: %s", string(out))

	// Verify hook ran
	data, err := os.ReadFile(marker)
	require.NoError(t, err, "hook should have created marker")
	assert.Contains(t, string(data), "rootfs=")

	// Cleanup
	cmd = exec.Command(runtimeBin, "kill", containerID, "SIGTERM")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	_, _ = cmd.CombinedOutput()
	time.Sleep(50 * time.Millisecond)

	cmd = exec.Command(runtimeBin, "delete", "--force", containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	_, _ = cmd.CombinedOutput()
}
