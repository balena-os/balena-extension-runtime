package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/proxy"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Seams for unit tests, mirroring create.go's proxy seams.
var (
	proxyStart = proxy.Start
	proxyFail  = proxy.Fail
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
		return abortStart(logger, state, fmt.Errorf("failed to read spec: %w", err))
	}

	rootfs, err := oci.ResolveRootfs(spec, state.Bundle)
	if err != nil {
		return abortStart(logger, state, fmt.Errorf("resolve rootfs: %w", err))
	}

	// The arm must be the last step that declines.
	err = hooks.ExecuteIfPresent(context.Background(), logger, rootfs, "hooks/start", state.Annotations, hookMounts(logger, containerID, spec.Mounts))
	if err == nil {
		err = activate(logger, containerID, rootfs, state.Annotations)
	}
	if err != nil {
		if !errors.Is(err, hooks.ErrRejected) {
			// Not the extension's fault: keep the container created.
			return abortStart(logger, state, err)
		}
		// The extension cannot activate, and no retry changes that.
		logger.Error("extension refused activation", "id", containerID, "err", err.Error())
		return stopContainer(logger, state, containerID, proxyFail, "Exited (1)")
	}

	return stopContainer(logger, state, containerID, proxyStart, "Exited (0)")
}

// abortStart terminates the proxy and returns the error that failed the start.
//
// A cleanup failure is logged, not returned. The caller is owed the error that
// failed the start, not this one.
func abortStart(logger *slog.Logger, state *specs.State, cause error) error {
	if err := proxyStop(state.Pid); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			logger.Info("proxy already exited before the start failed", "pid", state.Pid)
		} else {
			logger.Error("failed to stop proxy", "pid", state.Pid, "error", err)
		}
	}
	return cause
}

// stopContainer records the container's terminal state and signals the proxy
// to exit with the matching status, which is what the engine reports as the
// container's verdict.
func stopContainer(logger *slog.Logger, state *specs.State, containerID string, signal func(int) error, verdict string) error {
	state.Status = specs.StateStopped
	if err := oci.WriteState(state); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}

	if err := signal(state.Pid); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			logger.Info("proxy already exited before start signal", "pid", state.Pid)
		} else {
			return fmt.Errorf("failed to signal proxy: %w", err)
		}
	}

	logger.Info("container started and exited", "id", containerID, "verdict", verdict)
	return nil
}
