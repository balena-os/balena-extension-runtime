package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// createContainer creates a container from bundle and returns its ID with the
// pid of its proxy, asserting the proxy came up. The force-delete is registered
// as cleanup so a failed assertion mid-test cannot leave a proxy running.
func createContainer(t *testing.T, stateDir, bundle string) (string, int) {
	t.Helper()

	// Subtest names carry a "/", which ValidateContainerID rejects.
	containerID := strings.ReplaceAll(t.Name(), "/", "-") +
		"-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	pidFile := filepath.Join(t.TempDir(), "pid")

	cmd := exec.Command(runtimeBin, "create", "--bundle", bundle, "--pid-file", pidFile, containerID)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "create failed: %s", string(out))

	t.Cleanup(func() {
		cmd := exec.Command(runtimeBin, "delete", "--force", containerID)
		cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
		_, _ = cmd.CombinedOutput()
	})

	pidData, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(pidData))
	require.NoError(t, err)

	require.NoError(t, syscall.Kill(pid, 0), "proxy should be alive")

	return containerID, pid
}

// assertProxyGone polls instead of sleeping a fixed interval. Signal delivery
// and process teardown are asynchronous, so any single wait is either flaky on
// a loaded machine or longer than it needs to be.
func assertProxyGone(t *testing.T, pid int, msg string) {
	t.Helper()
	assert.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 2*time.Second, 10*time.Millisecond, msg)
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

// TestCreateRejectsKernelOverrideWithoutAnImageID pins the contract an
// extension claiming a kernel now creates under: its /boot volume is named
// after the image id, that id comes from the container store, and a store the
// runtime cannot read leaves the volume unnameable.
//
// Failing is the point. Carrying on would mint a name that collides across
// builds, and the name is the only route back to the volume: a redeploy reuses
// it, and cleanup's retention guard recognises the volume by it.
// Fabrication itself needs a live engine, so it is the integration suite that
// covers it; this is the half that can be pinned against the real binary.
func TestCreateRejectsKernelOverrideWithoutAnImageID(t *testing.T) {
	stateDir := t.TempDir()

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "sha256:abc123",
	})

	// A docker root with no entry for this container, which is what an
	// unreadable or misconfigured container store looks like from here.
	cmd := exec.Command(runtimeBin, "--docker-root", t.TempDir(),
		"create", "--bundle", bundle, "no-image-id-test")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "image id")
}

func TestKillProxy(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", stateDir)

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class": "overlay",
	})

	containerID, pid := createContainer(t, stateDir, bundle)

	cmd := exec.Command(runtimeBin, "kill", containerID, "SIGTERM")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "kill failed: %s", string(out))

	assertProxyGone(t, pid, "proxy should be dead after kill")
}

// TestKillProxyAll covers the invocation containerd issues on the force-delete
// path. Rejecting the flag made the engine return 500 on teardown and left the
// caller retrying against a container that could never be cleaned up.
func TestKillProxyAll(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", stateDir)

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class": "overlay",
	})

	containerID, pid := createContainer(t, stateDir, bundle)

	cmd := exec.Command(runtimeBin, "kill", "--all", containerID, "9")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+stateDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "kill --all failed: %s", string(out))

	assertProxyGone(t, pid, "proxy should be dead after kill --all")
}

func TestHookExecution(t *testing.T) {
	stateDir := t.TempDir()

	bundle := setupBundle(t, map[string]string{
		"io.balena.image.class": "overlay",
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
