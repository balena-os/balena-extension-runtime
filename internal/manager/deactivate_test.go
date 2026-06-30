package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeVolumeWithDeactivate creates a directory standing in for an extension
// volume's host source, containing an executable deactivate script.
func writeVolumeWithDeactivate(t *testing.T, script string) string {
	t.Helper()
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "deactivate"), []byte(script), 0o755))
	return src
}

// TestRunDeactivateHook_RunsFromVolumeWithLabelEnv proves the wiring: the
// manager runs <volume-source>/deactivate with the io.balena.image.* labels
// turned into env. The hook records EXTENSION_IMAGE_KERNEL_ABI_ID (its only
// real input) to a marker path passed through as a label-derived env var.
func TestRunDeactivateHook_RunsFromVolumeWithLabelEnv(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	const abi = "deadbeefabi"

	script := "#!/bin/sh\n" +
		`printf 'abi=%s rootfs=%s' "$EXTENSION_IMAGE_KERNEL_ABI_ID" "$EXTENSION_ROOTFS" > "$EXTENSION_IMAGE_TEST_MARKER"` + "\n"
	volSrc := writeVolumeWithDeactivate(t, script)

	c := Container{
		ID: "ko-container-1",
		Labels: map[string]string{
			"io.balena.image.class":         "overlay",
			"io.balena.image.kernel-abi-id": abi,
			"io.balena.image.test-marker":   marker,
		},
		Mounts: []MountPoint{{Type: "volume", Source: volSrc, Destination: "/boot"}},
	}

	require.NoError(t, runDeactivateHook(quietLogger(), c))

	got, err := os.ReadFile(marker)
	require.NoError(t, err, "deactivate hook must have run and written the marker")
	assert.Contains(t, string(got), "abi="+abi)
	assert.Contains(t, string(got), "rootfs="+volSrc, "EXTENSION_ROOTFS must be the volume source")
}

// TestRunDeactivateHook_NoVolumeWithHookIsNoop asserts an extension whose
// volumes carry no deactivate script is a clean no-op.
func TestRunDeactivateHook_NoVolumeWithHookIsNoop(t *testing.T) {
	c := Container{
		ID:     "plain-overlay",
		Labels: map[string]string{"io.balena.image.class": "overlay"},
		Mounts: []MountPoint{{Type: "volume", Source: t.TempDir(), Destination: "/data"}},
	}
	require.NoError(t, runDeactivateHook(quietLogger(), c))
}

// TestRunDeactivateHook_SkipsNonVolumeMounts asserts a deactivate script that
// happens to sit under a bind/tmpfs mount source is NOT run — only volumes
// are treated as extension-persistent storage.
func TestRunDeactivateHook_SkipsNonVolumeMounts(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	script := "#!/bin/sh\ntouch \"$EXTENSION_IMAGE_TEST_MARKER\"\n"
	bindSrc := writeVolumeWithDeactivate(t, script)

	c := Container{
		ID:     "bind-mount-ext",
		Labels: map[string]string{"io.balena.image.test-marker": marker},
		Mounts: []MountPoint{{Type: "bind", Source: bindSrc, Destination: "/boot"}},
	}
	require.NoError(t, runDeactivateHook(quietLogger(), c))

	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "deactivate under a bind mount must not run")
}

// TestCleanup_StaleOS_DeactivatesBeforeRemove is the integration check: a
// stale extension is deactivated (its volume's hook runs) before it is removed
// in the --stale-os prune pass.
func TestCleanup_StaleOS_DeactivatesBeforeRemove(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	const id = "stale-ko-container-0001"
	const abi = "staleabi123"

	script := "#!/bin/sh\n" +
		`echo "$EXTENSION_IMAGE_KERNEL_ABI_ID" > "$EXTENSION_IMAGE_TEST_MARKER"` + "\n"
	volSrc := writeVolumeWithDeactivate(t, script)

	stub := newEngineStub()
	stub.Containers = []Container{{
		ID:    id,
		Image: "ko-img",
		State: "exited",
		Labels: map[string]string{
			"io.balena.image.class":         "overlay",
			"io.balena.image.kernel-abi-id": abi,
			"io.balena.image.os-version":    "9.9.*", // never matches running -> stale
			"io.balena.image.test-marker":   marker,
		},
		Mounts: []MountPoint{{Type: "volume", Source: volSrc, Destination: "/boot"}},
	}}
	stub.Inspects[id] = inspectJSON(id, "exited", "", 0)
	testEngineEnv(t, testServer(t, stub.handler()))

	// Make VERSION_ID a value the container's os-version label cannot match.
	osr := filepath.Join(t.TempDir(), "os-release")
	require.NoError(t, os.WriteFile(osr, []byte("VERSION_ID=\"2.119.0\"\n"), 0o644))
	prev := osReleasePath
	osReleasePath = osr
	t.Cleanup(func() { osReleasePath = prev })

	err := Cleanup(context.Background(), quietLogger(), CleanupOpts{PruneStaleOS: true})
	require.NoError(t, err)

	got, err := os.ReadFile(marker)
	require.NoError(t, err, "deactivate must run before the stale container is removed")
	assert.Contains(t, string(got), abi)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Contains(t, stub.RemovedContainers, id, "stale container must still be removed")
}
