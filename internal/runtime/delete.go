package runtime

import (
	"fmt"
	"log/slog"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Delete removes the runtime state for a container.
func Delete(logger *slog.Logger, containerID string, force bool) error {
	if force {
		forceDelete(logger, containerID)
		return nil
	}
	return softDelete(logger, containerID)
}

func softDelete(logger *slog.Logger, containerID string) error {
	state, err := oci.ReadState(containerID)
	if err != nil {
		return fmt.Errorf("failed to read state: %w", err)
	}

	if state.Status == specs.StateRunning || state.Status == specs.StateCreated {
		return fmt.Errorf("cannot delete container %s in %s state (use --force to override)", containerID, state.Status)
	}

	if err := runDeleteHook(logger, state); err != nil {
		return err
	}

	return oci.RemoveState(containerID)
}

func forceDelete(logger *slog.Logger, containerID string) {
	state, err := oci.ReadState(containerID)
	if err != nil {
		logger.Error("failed to read state", "error", err)
		// State may not exist — still try cleanup
		_ = oci.RemoveState(containerID)
		return
	}

	if state.Status == specs.StateRunning || state.Status == specs.StateCreated {
		if err := Kill(logger, containerID, syscall.SIGKILL); err != nil {
			logger.Error("failed to kill container", "error", err)
		}
	}

	if err := runDeleteHook(logger, state); err != nil {
		logger.Error("delete hook failed", "error", err)
	}

	if err := oci.RemoveState(containerID); err != nil {
		logger.Error("failed to remove state", "error", err)
	}
}

func runDeleteHook(logger *slog.Logger, state *specs.State) error {
	spec, err := oci.ReadSpec(state.Bundle)
	if err != nil {
		return fmt.Errorf("failed to read spec: %w", err)
	}
	rootfs := oci.ResolveRootfs(spec, state.Bundle)
	return hooks.ExecuteIfPresent(logger, rootfs, "hooks/delete", state.Annotations)
}
