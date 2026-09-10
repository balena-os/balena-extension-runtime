package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Delete removes the runtime state for a container.
func Delete(logger *slog.Logger, containerID string, force bool) error {
	if force {
		return forceDelete(logger, containerID)
	}
	return softDelete(containerID)
}

func softDelete(containerID string) error {
	state, err := oci.ReadState(containerID)
	if err != nil {
		return fmt.Errorf("failed to read state: %w", err)
	}

	if state.Status == specs.StateRunning || state.Status == specs.StateCreated {
		return fmt.Errorf("cannot delete container %s in %s state (use --force to override)", containerID, state.Status)
	}

	return oci.RemoveState(containerID)
}

// forceDelete tears down a container even if individual steps fail. Each
// step runs regardless of prior failures; errors are accumulated and
// returned so containerd sees a non-zero exit on partial failure instead
// of a silently-swallowed cleanup.
func forceDelete(logger *slog.Logger, containerID string) error {
	var errs []error

	state, err := oci.ReadState(containerID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Called twice, or state already reaped.
	case err != nil:
		logger.Error("failed to read state", "error", err)
		errs = append(errs, fmt.Errorf("read state: %w", err))
	case state.Status == specs.StateRunning || state.Status == specs.StateCreated:
		if err := Kill(logger, containerID, syscall.SIGKILL); err != nil {
			logger.Error("failed to kill container", "error", err)
			errs = append(errs, fmt.Errorf("kill: %w", err))
		}
	}

	if err := oci.RemoveState(containerID); err != nil {
		logger.Error("failed to remove state", "error", err)
		errs = append(errs, fmt.Errorf("remove state: %w", err))
	}

	return errors.Join(errs...)
}
