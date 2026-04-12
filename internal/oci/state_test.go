package oci

import (
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateLifecycle(t *testing.T) {
	// Use temp dir as XDG_RUNTIME_DIR
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	containerID := "test-container-123"
	bundlePath := "/var/run/containerd/bundles/test"

	// Create
	state := NewState(containerID, bundlePath)
	assert.Equal(t, containerID, state.ID)
	assert.Equal(t, bundlePath, state.Bundle)
	assert.Equal(t, specs.StateCreating, state.Status)

	// Write
	state.Status = specs.StateCreated
	state.Pid = 42
	state.Annotations["io.balena.image.class"] = "overlay"
	require.NoError(t, WriteState(state))

	// Read
	loaded, err := ReadState(containerID)
	require.NoError(t, err)
	assert.Equal(t, containerID, loaded.ID)
	assert.Equal(t, bundlePath, loaded.Bundle)
	assert.Equal(t, specs.StateCreated, loaded.Status)
	assert.Equal(t, 42, loaded.Pid)
	assert.Equal(t, "overlay", loaded.Annotations["io.balena.image.class"])

	// Update
	loaded.Status = specs.StateStopped
	require.NoError(t, WriteState(loaded))

	reloaded, err := ReadState(containerID)
	require.NoError(t, err)
	assert.Equal(t, specs.StateStopped, reloaded.Status)

	// Remove
	require.NoError(t, RemoveState(containerID))

	_, err = ReadState(containerID)
	require.Error(t, err)
}

func TestReadStateNotFound(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	_, err := ReadState("nonexistent")
	require.Error(t, err)
}
