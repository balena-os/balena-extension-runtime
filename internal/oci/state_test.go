package oci

import (
	"strings"
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

func TestValidateContainerID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "valid alphanumeric", id: "abc123", wantErr: false},
		{name: "valid single char", id: "a", wantErr: false},
		{name: "valid mixed separators", id: "a_b.c-d", wantErr: false},
		{name: "valid starts with digit", id: "0abc", wantErr: false},
		{name: "valid max length", id: strings.Repeat("a", 1024), wantErr: false},
		{name: "invalid empty", id: "", wantErr: true},
		{name: "invalid too long", id: strings.Repeat("a", 1025), wantErr: true},
		{name: "invalid starts with underscore", id: "_abc", wantErr: true},
		{name: "invalid starts with dot", id: ".abc", wantErr: true},
		{name: "invalid starts with dash", id: "-abc", wantErr: true},
		{name: "invalid space", id: "a b", wantErr: true},
		{name: "invalid at sign", id: "a@b", wantErr: true},
		{name: "invalid slash", id: "a/b", wantErr: true},
		{name: "invalid newline", id: "a\nb", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateContainerID(tt.id)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestReadStateNotFound(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	_, err := ReadState("nonexistent")
	require.Error(t, err)
}
