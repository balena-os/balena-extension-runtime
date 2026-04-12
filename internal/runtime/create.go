package runtime

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/proxy"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Create validates the extension, runs the create hook, spawns the proxy,
// and writes the initial OCI state.
func Create(logger *slog.Logger, containerID string, bundlePath string, pidFile string) error {
	spec, err := oci.ReadSpec(bundlePath)
	if err != nil {
		return fmt.Errorf("failed to read OCI spec: %w", err)
	}

	// balena-engine does not copy container labels into OCI spec annotations.
	// Fall back to reading them from the Docker container store.
	oci.EnrichAnnotations(logger, spec, containerID)

	rootfs := oci.ResolveRootfs(spec, bundlePath)

	if err := labels.Validate(spec.Annotations); err != nil {
		return fmt.Errorf("invalid extension: %w", err)
	}

	if err := hooks.ExecuteIfPresent(logger, rootfs, "hooks/create", spec.Annotations); err != nil {
		return err
	}

	pid, err := proxy.NewProcess(containerID)
	if err != nil {
		return fmt.Errorf("failed to start proxy: %w", err)
	}
	needCleanup := true
	defer func() {
		if needCleanup {
			_ = proxy.Stop(pid)
		}
	}()

	state := oci.NewState(containerID, bundlePath)
	state.Pid = pid
	state.Status = specs.StateCreated
	state.Annotations = spec.Annotations
	if err := oci.WriteState(state); err != nil {
		return err
	}

	if pidFile != "" {
		if err := writePidFile(pidFile, pid); err != nil {
			return fmt.Errorf("failed to write pid file: %w", err)
		}
	}

	needCleanup = false
	logger.Info("container created", "id", containerID, "pid", pid)
	return nil
}

func writePidFile(path string, pid int) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", pid)), 0o644)
}
