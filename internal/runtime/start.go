package runtime

import (
	"fmt"
	"log/slog"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/proxy"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Start runs the start hook and signals the proxy to exit cleanly.
// The container transitions to "stopped" — this is intentional for extensions,
// which are overlay-only and don't run long-lived processes.
func Start(logger *slog.Logger, containerID string) error {
	state, err := oci.ReadState(containerID)
	if err != nil {
		return fmt.Errorf("failed to read state: %w", err)
	}

	if state.Status != specs.StateCreated {
		return fmt.Errorf("cannot start container %s in %s state", containerID, state.Status)
	}

	spec, err := oci.ReadSpec(state.Bundle)
	if err != nil {
		return fmt.Errorf("failed to read spec: %w", err)
	}

	rootfs, err := oci.ResolveRootfs(spec, state.Bundle)
	if err != nil {
		return fmt.Errorf("resolve rootfs: %w", err)
	}

	if err := hooks.ExecuteIfPresent(logger, rootfs, "hooks/start", state.Annotations); err != nil {
		return err
	}

	state.Status = specs.StateStopped
	if err := oci.WriteState(state); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}

	// Signal proxy to exit cleanly — container becomes "Exited (0)"
	if err := proxy.Start(state.Pid); err != nil {
		return fmt.Errorf("failed to signal proxy: %w", err)
	}

	logger.Info("container started and exited", "id", containerID)
	return nil
}
