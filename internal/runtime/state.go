package runtime

import (
	"fmt"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// State returns the current OCI state for a container.
func State(containerID string) (*specs.State, error) {
	state, err := oci.ReadState(containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to read state: %w", err)
	}
	return state, nil
}
