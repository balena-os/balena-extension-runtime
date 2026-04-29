package runtime

import (
	"fmt"
	"log/slog"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/proxy"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Kill sends a signal to the proxy process. For termination signals
// (SIGTERM/SIGKILL) delivery failures are tolerated — the proxy may have
// already exited — but are logged so they are not invisible.
func Kill(logger *slog.Logger, containerID string, signal syscall.Signal) error {
	state, err := oci.ReadState(containerID)
	if err != nil {
		return fmt.Errorf("failed to read state: %w", err)
	}

	if state.Pid > 0 {
		if err := proxy.Signal(state.Pid, signal); err != nil {
			if signal == syscall.SIGKILL || signal == syscall.SIGTERM {
				logger.Warn("signal delivery failed (proxy likely already gone)",
					"pid", state.Pid, "signal", signal, "err", err)
			} else {
				return fmt.Errorf("failed to send signal: %w", err)
			}
		}
	}

	state.Status = specs.StateStopped
	if err := oci.WriteState(state); err != nil {
		return fmt.Errorf("failed to write state: %w", err)
	}
	return nil
}
