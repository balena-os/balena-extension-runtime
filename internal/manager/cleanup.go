package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
)

// Cleanup removes extension containers that don't match the running kernel
// and dead containers. It then removes orphaned extension images.
func Cleanup(ctx context.Context, logger *slog.Logger) error {
	kver, err := runningKernelVersion()
	if err != nil {
		return err
	}
	logger.Info("cleaning up extensions", "kernel", kver)

	eng := NewEngine()

	containers, err := eng.ListContainers(ctx, labels.Class+"="+labels.ClassOverlay)
	if err != nil {
		return fmt.Errorf("list extension containers: %w", err)
	}

	// Track images that still have live containers.
	liveImages := make(map[string]bool)

	for _, c := range containers {
		// Remove dead containers unconditionally.
		if c.State == "dead" {
			logger.Info("removing dead extension container", "id", c.ID[:12])
			if err := eng.RemoveContainer(ctx, c.ID); err != nil {
				logger.Warn("failed to remove dead container", "id", c.ID[:12], "err", err)
			}
			continue
		}

		// Remove containers with a kernel-version label that doesn't match.
		cKver := c.Labels[labels.KernelVersion]
		if cKver != "" && cKver != kver {
			logger.Info("removing stale extension container", "id", c.ID[:12], "kernel", cKver)
			if err := eng.RemoveContainer(ctx, c.ID); err != nil {
				logger.Warn("failed to remove stale container", "id", c.ID[:12], "err", err)
			}
			continue
		}

		liveImages[c.Image] = true
	}

	// Remove orphaned extension images (no referencing containers).
	images, err := eng.ListImages(ctx, labels.Class+"="+labels.ClassOverlay)
	if err != nil {
		logger.Warn("failed to list extension images", "err", err)
		return nil
	}
	for _, img := range images {
		if liveImages[img.ID] {
			continue
		}
		// Also check by repo tag since container.Image may be a tag.
		tagMatch := false
		for _, tag := range img.RepoTags {
			if liveImages[tag] {
				tagMatch = true
				break
			}
		}
		if tagMatch {
			continue
		}
		logger.Info("removing orphaned extension image", "id", img.ID[:12])
		if err := eng.RemoveImage(ctx, img.ID); err != nil {
			logger.Warn("failed to remove orphaned image", "id", img.ID[:12], "err", err)
		}
	}
	return nil
}

// runningKernelVersion returns the M.m.p portion of the running kernel.
func runningKernelVersion() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "", fmt.Errorf("read kernel version: %w", err)
	}
	release := strings.TrimSpace(string(data))
	if idx := strings.IndexByte(release, '-'); idx > 0 {
		release = release[:idx]
	}
	return release, nil
}
