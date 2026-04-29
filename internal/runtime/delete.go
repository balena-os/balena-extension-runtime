package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Delete removes the runtime state for a container.
func Delete(logger *slog.Logger, containerID string, force bool) error {
	if force {
		return forceDelete(logger, containerID)
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

// forceDelete tears down a container even if individual steps fail. Each
// step runs regardless of prior failures; errors are accumulated and
// returned so containerd sees a non-zero exit on partial failure instead
// of a silently-swallowed cleanup.
func forceDelete(logger *slog.Logger, containerID string) error {
	var errs []error

	state, err := oci.ReadState(containerID)
	if err != nil {
		// Missing state is the idempotent case (force-delete called
		// twice, or state already reaped) — not an error. Any other
		// ReadState failure (corrupt JSON, permission) is worth surfacing.
		if !errors.Is(err, os.ErrNotExist) {
			logger.Error("failed to read state", "error", err)
			errs = append(errs, fmt.Errorf("read state: %w", err))
		}
		if rmErr := oci.RemoveState(containerID); rmErr != nil {
			logger.Error("failed to remove state", "error", rmErr)
			errs = append(errs, fmt.Errorf("remove state: %w", rmErr))
		}
		return errors.Join(errs...)
	}

	if state.Status == specs.StateRunning || state.Status == specs.StateCreated {
		if err := Kill(logger, containerID, syscall.SIGKILL); err != nil {
			logger.Error("failed to kill container", "error", err)
			errs = append(errs, fmt.Errorf("kill: %w", err))
		}
	}

	if err := runDeleteHook(logger, state); err != nil {
		logger.Error("delete hook failed", "error", err)
		errs = append(errs, fmt.Errorf("delete hook: %w", err))
	}

	if err := oci.RemoveState(containerID); err != nil {
		logger.Error("failed to remove state", "error", err)
		errs = append(errs, fmt.Errorf("remove state: %w", err))
	}

	return errors.Join(errs...)
}

func runDeleteHook(logger *slog.Logger, state *specs.State) error {
	spec, err := oci.ReadSpec(state.Bundle)
	if err != nil {
		return fmt.Errorf("failed to read spec: %w", err)
	}
	rootfs, err := oci.ResolveRootfs(spec, state.Bundle)
	if err != nil {
		return fmt.Errorf("resolve rootfs: %w", err)
	}
	return hooks.ExecuteIfPresent(logger, rootfs, "hooks/delete", state.Annotations)
}
