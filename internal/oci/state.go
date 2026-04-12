package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/opencontainers/runtime-spec/specs-go"
)

var validContainerID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]*$`)

// ValidateContainerID checks that a container ID is safe for use in filesystem paths.
func ValidateContainerID(id string) error {
	if id == "" {
		return fmt.Errorf("container ID must not be empty")
	}
	if len(id) > 1024 {
		return fmt.Errorf("container ID exceeds maximum length of 1024")
	}
	if !validContainerID.MatchString(id) {
		return fmt.Errorf("invalid container ID %q: must match [a-zA-Z0-9][a-zA-Z0-9_.\\-]*", id)
	}
	return nil
}

const (
	stateFileName = "state.json"
	runtimeName   = "balena-extension-runtime"
)

var stateRoot string

// SetStateRoot overrides the default state directory. It should be called with
// the value of the --root global flag (passed by containerd) before any state
// operations. If not set, the default path under XDG_RUNTIME_DIR or /run is used.
func SetStateRoot(root string) {
	stateRoot = root
}

func getStateDir() string {
	if stateRoot != "" {
		return stateRoot
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run"
	}
	return filepath.Join(dir, runtimeName)
}

// NewState creates a fresh OCI state for a container in the "creating" status.
func NewState(containerID string, bundlePath string) *specs.State {
	return &specs.State{
		Version:     specs.Version,
		ID:          containerID,
		Status:      specs.StateCreating,
		Pid:         0,
		Bundle:      bundlePath,
		Annotations: map[string]string{},
	}
}

// WriteState persists the OCI state atomically to disk.
func WriteState(state *specs.State) error {
	if err := ValidateContainerID(state.ID); err != nil {
		return err
	}
	containerDir := filepath.Join(getStateDir(), state.ID)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	stateFile := filepath.Join(containerDir, stateFileName)
	return atomicWrite(stateFile, data)
}

// ReadState loads the OCI state for a container.
func ReadState(containerID string) (*specs.State, error) {
	if err := ValidateContainerID(containerID); err != nil {
		return nil, err
	}
	stateFile := filepath.Join(getStateDir(), containerID, stateFileName)
	f, err := os.Open(stateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open state file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var s specs.State
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, fmt.Errorf("failed to decode state: %w", err)
	}
	return &s, nil
}

// RemoveState deletes the state directory for a container.
func RemoveState(containerID string) error {
	if err := ValidateContainerID(containerID); err != nil {
		return err
	}
	containerDir := filepath.Join(getStateDir(), containerID)
	if err := os.RemoveAll(containerDir); err != nil {
		return fmt.Errorf("failed to remove state directory: %w", err)
	}
	return nil
}

func atomicWrite(filePath string, content []byte) error {
	tmp := filePath + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := os.Rename(tmp, filePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	return nil
}
