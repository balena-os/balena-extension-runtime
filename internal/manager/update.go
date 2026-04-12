package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
)

// Update re-creates extension containers with the kernel version from the new
// rootfs. Called during HUP (host OS update) so that after reboot mobynit
// mounts only ABI-compatible extensions.
func Update(ctx context.Context, logger *slog.Logger, rootfs string) error {
	kver, err := readKernelVersion(rootfs)
	if err != nil {
		return err
	}
	logger.Info("installing extensions for new kernel", "kernel", kver)

	eng := NewEngine()

	containers, err := eng.ListContainers(ctx, labels.Class+"="+labels.ClassOverlay)
	if err != nil {
		return fmt.Errorf("list extension containers: %w", err)
	}

	for _, c := range containers {
		image := c.Image
		newLabels := map[string]string{
			labels.Class:         labels.ClassOverlay,
			labels.KernelVersion: kver,
		}

		id, err := eng.CreateContainer(ctx, image, "extension", newLabels, []string{"none"})
		if err != nil {
			logger.Warn("failed to create extension container", "image", image, "err", err)
			continue
		}
		if err := eng.StartContainer(ctx, id); err != nil {
			logger.Warn("failed to start extension container", "id", id, "err", err)
			continue
		}
		logger.Info("extension updated", "image", image, "kernel", kver)
	}
	return nil
}

// readKernelVersion extracts the kernel version (M.m.p) from the new rootfs
// by looking at /lib/modules/<version>/.
//
// This function is called on a freshly-extracted balenaOS rootfs during HUP —
// balenaOS images ship with exactly one kernel, so /lib/modules contains a
// single numeric-prefixed directory. We return the first one we find; on a
// well-formed rootfs it is also the only one. If a rootfs unexpectedly
// contains multiple kernels (custom build, corruption), os.ReadDir returns
// entries sorted by name, so the behaviour is deterministic but not
// semantically meaningful — callers must guarantee a single-kernel rootfs.
func readKernelVersion(rootfs string) (string, error) {
	modulesDir := filepath.Join(rootfs, "lib", "modules")
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", modulesDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		// Strip suffix after first dash: "6.6.74-v8" -> "6.6.74"
		if idx := strings.IndexByte(name, '-'); idx > 0 {
			name = name[:idx]
		}
		return name, nil
	}
	return "", fmt.Errorf("no kernel version found in %s", modulesDir)
}
