package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/proxy"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// proxySpawnTimeout bounds how long we wait on the proxy spawn itself (fork +
// exec). Once Start returns the proxy detaches and lives independently, so
// this only guards against a wedged fork/exec — not the proxy's lifetime.
const proxySpawnTimeout = 10 * time.Second

// Test seams: tests override these to assert the cleanup defer is wired up
// without spawning a real subprocess. Production code keeps the proxy
// package implementations.
var (
	proxyNewProcess = proxy.NewProcess
	proxyStop       = proxy.Stop
)

// Create validates the extension, runs the create hook, spawns the proxy,
// and writes the initial OCI state. ctx bounds the proxy spawn and lets the
// caller (typically containerd via SIGTERM) cancel an in-flight create.
func Create(ctx context.Context, logger *slog.Logger, containerID string, bundlePath string, pidFile string) error {
	bundlePath, err := oci.NormalizeBundlePath(bundlePath)
	if err != nil {
		return fmt.Errorf("invalid bundle path: %w", err)
	}

	spec, err := oci.ReadSpec(bundlePath)
	if err != nil {
		return fmt.Errorf("failed to read OCI spec: %w", err)
	}

	// balena-engine does not copy container labels into OCI spec annotations.
	stored := oci.EnrichAnnotations(logger, spec, containerID)

	rootfs, err := oci.ResolveRootfs(spec, bundlePath)
	if err != nil {
		return fmt.Errorf("resolve rootfs: %w", err)
	}

	if err := labels.Validate(spec.Annotations); err != nil {
		return fmt.Errorf("invalid extension: %w", err)
	}

	// Fabricate before the create hook rather than after it: the hook is
	// what publishes the kernel out of the volume, so it has to find the
	// volume filled.
	bootVolume, err := fabricateBootVolume(ctx, logger, spec, stored, rootfs, containerID)
	if err != nil {
		return fmt.Errorf("fabricate boot volume: %w", err)
	}

	// A non-zero exit fails the create call instead of recording a verdict:
	// the proxy whose exit status carries the container's outcome does not
	// exist yet. Extensions decline activation from hooks/start, where there
	// is a process to carry the decision.
	if err := hooks.ExecuteIfPresent(ctx, logger, rootfs, "hooks/create", spec.Annotations, withBootVolume(spec.Mounts, bootVolume)); err != nil {
		return err
	}

	spawnCtx, cancel := context.WithTimeout(ctx, proxySpawnTimeout)
	defer cancel()
	pid, err := proxyNewProcess(spawnCtx, containerID)
	if err != nil {
		return fmt.Errorf("failed to start proxy: %w", err)
	}
	needCleanup := true
	defer func() {
		if needCleanup {
			if stopErr := proxyStop(pid); stopErr != nil {
				logger.Warn("failed to stop proxy during cleanup", "pid", pid, "err", stopErr)
			}
			// Remove any partial state so a stale Pid can't be signalled
			// later. RemoveState is a no-op if WriteState never ran.
			if rmErr := oci.RemoveState(containerID); rmErr != nil {
				logger.Warn("failed to remove partial state during cleanup", "id", containerID, "err", rmErr)
			}
		}
	}()

	state := oci.NewState(containerID, bundlePath)
	state.Pid = pid
	state.Status = specs.StateCreated
	state.Annotations = spec.Annotations
	if err := oci.WriteState(state); err != nil {
		return err
	}
	if bootVolume != "" {
		if err := oci.WriteBootVolume(containerID, bootVolume); err != nil {
			return err
		}
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
