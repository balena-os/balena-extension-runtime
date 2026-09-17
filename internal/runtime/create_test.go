package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProxy swaps the proxy spawn/stop functions for the duration of a
// test. It records the PIDs passed to Stop so the cleanup defer can be
// asserted without running a real subprocess.
type fakeProxy struct {
	spawnPID    int
	spawnErr    error
	spawnCalls  int
	stoppedPIDs []int
	stopErr     error
}

func (f *fakeProxy) install(t *testing.T) {
	t.Helper()
	prevNew, prevStop := proxyNewProcess, proxyStop
	proxyNewProcess = func(ctx context.Context, containerID string) (int, error) {
		f.spawnCalls++
		return f.spawnPID, f.spawnErr
	}
	proxyStop = func(pid int) error {
		f.stoppedPIDs = append(f.stoppedPIDs, pid)
		return f.stopErr
	}
	t.Cleanup(func() {
		proxyNewProcess = prevNew
		proxyStop = prevStop
	})
}

// validBundleWithAnnotations writes an OCI config.json with enough
// annotations to pass labels.Validate and a real rootfs directory so
// ResolveRootfs succeeds.
func validBundleWithAnnotations(t *testing.T) string {
	t.Helper()
	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))

	spec := specs.Spec{
		Version: specs.Version,
		Root:    &specs.Root{Path: "rootfs"},
		Annotations: map[string]string{
			"io.balena.image.class": "overlay",
		},
	}
	data, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644))
	return bundle
}

// TestCreate_WriteStateFailure_StopsProxy asserts the needCleanup defer
// fires when WriteState rejects an invalid container ID: the spawned
// proxy must be stopped, not leaked.
func TestCreate_WriteStateFailure_StopsProxy(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	fp := &fakeProxy{spawnPID: 4242}
	fp.install(t)

	bundle := validBundleWithAnnotations(t)

	// WriteState rejects the '/'; every step above it passes.
	err := Create(context.Background(), testLogger(), "bad/id", bundle, "")
	require.Error(t, err)

	assert.Equal(t, []int{4242}, fp.stoppedPIDs,
		"proxy must be stopped when WriteState fails")
}

// TestCreate_PidFileFailure_StopsProxy asserts the defer also fires when
// pid-file writing fails after WriteState succeeds — another path where a
// regression could leak the proxy.
func TestCreate_PidFileFailure_StopsProxy(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	fp := &fakeProxy{spawnPID: 9999}
	fp.install(t)

	bundle := validBundleWithAnnotations(t)
	// Directory path as pid-file target causes os.WriteFile to fail.
	pidFile := t.TempDir()

	err := Create(context.Background(), testLogger(), "good-id", bundle, pidFile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pid file")

	assert.Equal(t, []int{9999}, fp.stoppedPIDs,
		"proxy must be stopped when pid-file write fails")
}

// TestCreate_Success_DoesNotStopProxy asserts the happy path leaves the
// proxy running — the cleanup defer must remain off once WriteState and
// pid-file both succeed.
func TestCreate_Success_DoesNotStopProxy(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	fp := &fakeProxy{spawnPID: 7777}
	fp.install(t)

	bundle := validBundleWithAnnotations(t)
	pidFile := filepath.Join(t.TempDir(), "runtime.pid")

	err := Create(context.Background(), testLogger(), "good-id", bundle, pidFile)
	require.NoError(t, err)

	assert.Empty(t, fp.stoppedPIDs, "proxy must not be stopped on success")

	// State should record the spawned PID so Start/Kill/Delete can reach it.
	state, err := oci.ReadState("good-id")
	require.NoError(t, err)
	assert.Equal(t, 7777, state.Pid)
	assert.Equal(t, specs.StateCreated, state.Status)
}

// TestCreate_SpawnFailure_NoStopCalled asserts that a failed proxy spawn
// does not invoke Stop — there's no pid to stop, and calling Stop(-1)
// would spuriously signal pid -1.
func TestCreate_SpawnFailure_NoStopCalled(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	fp := &fakeProxy{spawnPID: -1, spawnErr: context.DeadlineExceeded}
	fp.install(t)

	bundle := validBundleWithAnnotations(t)

	err := Create(context.Background(), testLogger(), "good-id", bundle, "")
	require.Error(t, err)

	assert.Empty(t, fp.stoppedPIDs,
		"Stop must not be invoked when proxy spawn itself failed")
}

// TestCreate_FabricationFailure_NoProxySpawned asserts a kernel-claiming
// extension that fails to fabricate its /boot volume never reaches the
// proxy: fabrication runs before the spawn, so this failure leaves nothing
// to clean up.
func TestCreate_FabricationFailure_NoProxySpawned(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	fp := &fakeProxy{spawnPID: 6161}
	fp.install(t)

	// An empty store has no image id, so fabrication fails.
	oci.SetDockerRoot(t.TempDir())
	t.Cleanup(func() { oci.SetDockerRoot("/var/lib/docker") })

	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))

	annotations := map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-test",
		"io.balena.service-name":        "kernel-modules",
	}
	spec := specs.Spec{
		Version:     specs.Version,
		Root:        &specs.Root{Path: "rootfs"},
		Annotations: annotations,
	}
	data, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644))

	containerID := "fabrication-fail-test"
	err = Create(context.Background(), testLogger(), containerID, bundle, "")
	require.ErrorContains(t, err, "fabricate boot volume")

	assert.Zero(t, fp.spawnCalls, "proxy must not be spawned when fabrication fails")

	_, readErr := oci.ReadState(containerID)
	require.Error(t, readErr, "no state should be written when fabrication fails")
}

// writeStore lays down the container store record the engine keeps, which is
// the identity ReadIdentity resolves.
func writeStore(t *testing.T, dockerRoot, containerID, imageID string, lbls map[string]string) {
	t.Helper()
	dir := filepath.Join(dockerRoot, "containers", containerID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	config := map[string]any{"Image": imageID, "Config": map[string]any{"Labels": lbls}}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.v2.json"), data, 0o644))
}

// TestCreate_RecordsTheStoreIdentity pins which map the rest of the lifecycle
// reads. The engine's labels admit the extension and name its volume. The
// bundle's annotations disagree and change neither.
func TestCreate_RecordsTheStoreIdentity(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	fp := &fakeProxy{spawnPID: 5150}
	fp.install(t)

	stub := newStubEngine(t)
	const containerID = "store-identity"
	storeLabels := map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-integration",
		"io.balena.service-name":        "kernel-modules",
	}
	writeStore(t, stub.root, containerID, "sha256:42befc76f4f8aaaa", storeLabels)

	bundle := t.TempDir()
	rootfs := filepath.Join(bundle, "rootfs")
	require.NoError(t, os.MkdirAll(rootfs, 0o755))
	spec := specs.Spec{
		Version: specs.Version,
		Root:    &specs.Root{Path: "rootfs"},
		// The bundle names a different service and claims no kernel.
		Annotations: map[string]string{
			"io.balena.image.class":  "overlay",
			"io.balena.service-name": "from-the-bundle",
		},
	}
	data, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644))

	require.NoError(t, Create(context.Background(), testLogger(), containerID, bundle, ""))

	state, err := oci.ReadState(containerID)
	require.NoError(t, err)
	assert.Equal(t, storeLabels, state.Annotations)

	name, err := oci.ReadBootVolume(containerID)
	require.NoError(t, err)
	assert.Equal(t, "ext_kernel-modules_42befc76f4f8_boot", name)
	assert.Equal(t, []string{name}, stub.order, "the store's labels name the volume")
}
