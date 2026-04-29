package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
)

// osReleasePath is the default path to /etc/os-release. Overridable in tests.
var osReleasePath = "/etc/os-release"

// CleanupOpts configures what Cleanup removes.
type CleanupOpts struct {
	// PruneStaleOS enables the stale-OS pass: in addition to the
	// always-on dead-container sweep, containers and images whose
	// declared compatibility claims are violated by the running system
	// are removed. Only safe after HUP rollback-health commit; outside
	// that window, stale containers/images are the rollback target and
	// must be preserved.
	PruneStaleOS bool
}

// Cleanup removes extension containers and images. The dead-container
// sweep always runs. When opts.PruneStaleOS is set, the stale predicate is
// additionally applied — symmetrically to containers and images — using
// three compatibility levels, all of which must be satisfied if claimed:
//
//   - io.balena.image.kernel-abi-id (kernel-space ABI: symbol CRCs)
//   - io.balena.image.kernel-version (userspace-to-kernel ABI)
//   - io.balena.image.os-version (OS compatibility: libc, paths, hostapp)
//
// Absent labels make no claim at that level.
func Cleanup(ctx context.Context, logger *slog.Logger, opts CleanupOpts) error {
	eng := NewEngine()
	if err := eng.CheckSocket(); err != nil {
		return err
	}

	containers, err := eng.ListContainers(ctx, labels.Class+"="+labels.ClassOverlay)
	if err != nil {
		return fmt.Errorf("list extension containers: %w", err)
	}

	// Per-removal failures are accumulated rather than short-circuiting so
	// one bad container doesn't block cleanup of the rest. The aggregated
	// errors are returned so the caller (typically a systemd unit) surfaces
	// a non-zero exit on partial failure instead of masking it as a warning.
	var removalErrs []error

	// Dead sweep runs in both modes.
	dropped := make(map[string]bool)
	for _, c := range containers {
		if c.State != "dead" {
			continue
		}
		logger.Info("removing dead extension container", "id", c.ID[:12])
		if err := eng.RemoveContainer(ctx, c.ID); err != nil {
			logger.Warn("failed to remove dead container", "id", c.ID[:12], "err", err)
			removalErrs = append(removalErrs, fmt.Errorf("remove dead container %s: %w", c.ID[:12], err))
			continue
		}
		dropped[c.ID] = true
	}

	if !opts.PruneStaleOS {
		logger.Info("cleaning up dead extensions")
		return errors.Join(removalErrs...)
	}

	kver, err := runningKernelVersion()
	if err != nil {
		return errors.Join(append(removalErrs, fmt.Errorf("read running kernel version: %w", err))...)
	}
	// A failure here is distinct from the legitimate "" result that
	// runningKernelABIID returns when Module.symvers is absent: we can't
	// tell if abi-labelled images match the running kernel. The caller
	// explicitly asked for a stale-OS sweep, so a failure to compute the
	// predicate is returned as an error — silently degrading to dead-only
	// mode would let disks fill with stale extensions after a HUP commit
	// without anyone noticing.
	abiID, err := runningKernelABIID()
	if err != nil {
		return errors.Join(append(removalErrs, fmt.Errorf("compute kernel ABI ID: %w", err))...)
	}
	osVersion, err := readOSVersion()
	if err != nil {
		return errors.Join(append(removalErrs, fmt.Errorf("read OS version: %w", err))...)
	}
	logger.Info("cleaning up stale extensions",
		"kernel-version", kver,
		"kernel-abi-id", abiID,
		"os-version", osVersion,
	)

	for _, c := range containers {
		if dropped[c.ID] {
			continue
		}
		if !stale(logger, c.Labels, kver, abiID, osVersion) {
			continue
		}
		logger.Info("removing stale extension container",
			"id", c.ID[:12],
			"kernel-version", c.Labels[labels.KernelVersion],
			"kernel-abi-id", c.Labels[labels.KernelABIID],
			"os-version", c.Labels[labels.OSVersion],
		)
		if err := eng.RemoveContainer(ctx, c.ID); err != nil {
			logger.Warn("failed to remove stale container", "id", c.ID[:12], "err", err)
			removalErrs = append(removalErrs, fmt.Errorf("remove stale container %s: %w", c.ID[:12], err))
		}
	}

	images, err := eng.ListImages(ctx, labels.Class+"="+labels.ClassOverlay)
	if err != nil {
		return errors.Join(append(removalErrs, fmt.Errorf("list extension images: %w", err))...)
	}
	for _, img := range images {
		if !stale(logger, img.Labels, kver, abiID, osVersion) {
			continue
		}
		logger.Info("removing stale extension image",
			"id", img.ID[:12],
			"kernel-version", img.Labels[labels.KernelVersion],
			"kernel-abi-id", img.Labels[labels.KernelABIID],
			"os-version", img.Labels[labels.OSVersion],
		)
		if err := eng.RemoveImage(ctx, img.ID); err != nil {
			logger.Warn("failed to remove stale image", "id", img.ID[:12], "err", err)
			removalErrs = append(removalErrs, fmt.Errorf("remove stale image %s: %w", img.ID[:12], err))
		}
	}
	return errors.Join(removalErrs...)
}

// stale reports whether any compatibility claim the labels declare is
// violated by the running system. Absent labels make no claim at that
// level. Applied symmetrically to containers and images.
//
// Checks are independent — all declared claims must hold:
//   - kernel-abi-id: fails when runningAbi can't verify the claim or
//     doesn't equal it.
//   - kernel-version: fails when running kernel M.m.p differs.
//   - os-version: comma-separated globs against running VERSION_ID.
func stale(logger *slog.Logger, lbls map[string]string, runningKver, runningAbi, runningOsVersion string) bool {
	if abi := lbls[labels.KernelABIID]; abi != "" && abi != runningAbi {
		return true
	}
	if kver := lbls[labels.KernelVersion]; kver != "" && kver != runningKver {
		return true
	}
	if osLabel := lbls[labels.OSVersion]; osLabel != "" &&
		!osVersionMatch(logger, osLabel, runningOsVersion) {
		return true
	}
	return false
}

// osVersionMatch reports whether the running OS version satisfies the
// io.balena.image.os-version label. An empty label is treated as a retain
// (legacy-safe default). The label is a comma-separated list of shell-style
// globs; any match retains the image.
func osVersionMatch(logger *slog.Logger, label, running string) bool {
	patterns := make([]string, 0)
	for _, pat := range strings.Split(label, ",") {
		if pat = strings.TrimSpace(pat); pat != "" {
			patterns = append(patterns, pat)
		}
	}
	if len(patterns) == 0 {
		return true
	}
	for _, pat := range patterns {
		ok, err := filepath.Match(pat, running)
		if err != nil {
			// Malformed pattern — we can't verify the claim, so retain
			// rather than mark the image stale and delete it. Surface the
			// pattern so an extension author who typoed the os-version
			// label can diagnose why images are never pruned.
			logger.Warn("malformed os-version pattern in extension label; retaining",
				"label", label, "pattern", pat, "err", err)
			return true
		}
		if ok {
			return true
		}
	}
	return false
}

// readOSVersion returns VERSION_ID from /etc/os-release.
func readOSVersion() (string, error) {
	return readOSVersionFrom(osReleasePath)
}

// readOSVersionFrom parses VERSION_ID from a path (test seam).
func readOSVersionFrom(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "VERSION_ID=") {
			v := strings.TrimPrefix(line, "VERSION_ID=")
			v = strings.Trim(v, `"'`)
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("VERSION_ID not found in %s", path)
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

// runningKernelABIID computes the sha256 of the running kernel's
// Module.symvers. Returns "" with nil error if the file does not exist —
// extensions that claim kernel-abi-id against such a device will fail
// their claim naturally through the `stale` predicate.
func runningKernelABIID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "", fmt.Errorf("read kernel release: %w", err)
	}
	release := strings.TrimSpace(string(data))
	symvers := filepath.Join("/lib/modules", release, "Module.symvers")
	content, err := os.ReadFile(symvers)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Module.symvers: %w", err)
	}
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:]), nil
}
