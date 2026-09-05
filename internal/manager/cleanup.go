package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/hostapp"
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

// Cleanup collects the extension objects nothing on the device claims.
//
// The unconditional pass removes the containers the engine calls garbage, and
// the fabricated volumes no surviving container claims. Its claim source is
// the engine's container list.
//
// opts.PruneStaleOS adds a second pass over containers and images, on stale().
func Cleanup(ctx context.Context, logger *slog.Logger, opts CleanupOpts) error {
	return WithOperationLock(ctx, func() error {
		return cleanup(ctx, logger, opts)
	})
}

// cleanup is Cleanup's implementation, run with the operation lock held.
func cleanup(ctx context.Context, logger *slog.Logger, opts CleanupOpts) error {
	eng := NewEngine()
	if err := eng.CheckSocket(); err != nil {
		return err
	}

	// Snapshotted before the container list, never the other way round: that
	// order is the whole proof that a volume this sweep collects is
	// unreferenced. A fabricated volume is dangling from birth, so the filter
	// only trims the response and the engine's in-use protection never applies.
	vols, volsErr := eng.ListVolumes(ctx, true)

	containers, err := eng.ListContainers(ctx, labels.Class+"="+labels.ClassOverlay)
	if err != nil {
		return fmt.Errorf("list extension containers: %w", err)
	}

	// Per-removal failures are accumulated rather than short-circuiting so
	// one bad container doesn't block cleanup of the rest. The aggregated
	// errors are returned so the caller (typically a systemd unit) surfaces
	// a non-zero exit on partial failure instead of masking it as a warning.
	var removalErrs []error
	dropped := make(map[string]bool)

	logger.Info("collecting garbage extension containers")
	for _, c := range containers {
		reason := garbageReason(ctx, logger, eng, c)
		if reason == "" {
			continue
		}
		logger.Info("removing extension container",
			"id", labels.ShortID(c.ID), "state", c.State, "reason", reason)
		if err := eng.RemoveContainer(ctx, logger, c.ID); err != nil {
			logger.Warn("failed to remove extension container",
				"id", labels.ShortID(c.ID), "err", err)
			removalErrs = append(removalErrs,
				fmt.Errorf("remove container %s: %w", labels.ShortID(c.ID), err))
			continue
		}
		dropped[c.ID] = true
	}

	if opts.PruneStaleOS {
		removalErrs = append(removalErrs, pruneStaleOS(ctx, logger, eng, containers, dropped)...)
	}

	// After every container pass, so a volume freed above is collected in this
	// run rather than at the next boot.
	if volsErr != nil {
		return errors.Join(append(removalErrs, fmt.Errorf("list dangling volumes: %w", volsErr))...)
	}
	claimed, err := claimedVolumes(containers, dropped)
	if err != nil {
		return errors.Join(append(removalErrs, fmt.Errorf("derive volume claims: %w", err))...)
	}
	for _, v := range vols {
		if v.Labels[labels.Class] != labels.ClassOverlay {
			continue
		}
		if claimed[v.Name] {
			logger.Info("retaining extension volume, a container still claims it", "name", v.Name)
			continue
		}
		logger.Info("removing unclaimed extension volume", "name", v.Name)
		if err := eng.RemoveVolume(ctx, v.Name); err != nil {
			logger.Warn("failed to remove unclaimed volume", "name", v.Name, "err", err)
			removalErrs = append(removalErrs, fmt.Errorf("remove volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(removalErrs...)
}

// pruneStaleOS removes the containers and images whose declared compatibility
// claims the running system violates.
//
// A predicate it cannot compute fails rather than degrading to a no-op:
// skipping a sweep the caller asked for would let disks fill after a HUP
// commit with nobody noticing.
func pruneStaleOS(ctx context.Context, logger *slog.Logger, eng *Engine, containers []Container, dropped map[string]bool) []error {
	kver, err := runningKernelVersion()
	if err != nil {
		return []error{fmt.Errorf("read running kernel version: %w", err)}
	}
	// A failure here is distinct from the legitimate "" result that
	// runningKernelABIID returns when the balena_kernel_abi cmdline token is
	// absent: it means we cannot tell whether abi-labelled images match the
	// running kernel.
	abiID, err := runningKernelABIID()
	if err != nil {
		return []error{fmt.Errorf("compute kernel ABI ID: %w", err)}
	}
	osVersion, err := readOSVersion()
	if err != nil {
		return []error{fmt.Errorf("read OS version: %w", err)}
	}
	logger.Info("removing stale extensions",
		"kernel-version", kver,
		"kernel-abi-id", abiID,
		"os-version", osVersion,
	)

	var errs []error
	for _, c := range containers {
		if dropped[c.ID] {
			continue
		}
		if !stale(logger, c.Labels, kver, abiID, osVersion) {
			continue
		}
		logger.Info("removing stale extension container",
			"id", labels.ShortID(c.ID),
			"kernel-version", c.Labels[labels.KernelVersion],
			"kernel-abi-id", c.Labels[labels.KernelABIID],
			"os-version", c.Labels[labels.OSVersion],
		)
		if err := eng.RemoveContainer(ctx, logger, c.ID); err != nil {
			logger.Warn("failed to remove stale container", "id", labels.ShortID(c.ID), "err", err)
			errs = append(errs, fmt.Errorf("remove stale container %s: %w", labels.ShortID(c.ID), err))
			continue
		}
		dropped[c.ID] = true
	}

	images, err := eng.ListImages(ctx, labels.Class+"="+labels.ClassOverlay)
	if err != nil {
		return append(errs, fmt.Errorf("list extension images: %w", err))
	}
	for _, img := range images {
		if !stale(logger, img.Labels, kver, abiID, osVersion) {
			continue
		}
		logger.Info("removing stale extension image",
			"id", labels.ShortID(img.ID),
			"kernel-version", img.Labels[labels.KernelVersion],
			"kernel-abi-id", img.Labels[labels.KernelABIID],
			"os-version", img.Labels[labels.OSVersion],
		)
		if err := eng.RemoveImage(ctx, img.ID); err != nil {
			logger.Warn("failed to remove stale image", "id", labels.ShortID(img.ID), "err", err)
			errs = append(errs, fmt.Errorf("remove stale image %s: %w", labels.ShortID(img.ID), err))
		}
	}
	return errs
}

// garbageReason says why the engine's own account of a container makes it
// collectable, or "" when it does not.
//
// An inspect that fails leaves the container alone: removing on a failed
// inspect would be removing on no evidence at all.
func garbageReason(ctx context.Context, logger *slog.Logger, eng *Engine, c Container) string {
	if c.State == "dead" {
		return "the engine reports it dead"
	}
	if c.State != "created" && c.State != "exited" {
		return ""
	}
	ci, err := eng.InspectContainer(ctx, c.ID)
	if err != nil {
		logger.Warn("failed to inspect container, leaving it alone",
			"id", labels.ShortID(c.ID), "err", err)
		return ""
	}
	if ci.State.Error == "" {
		return ""
	}
	return fmt.Sprintf("its runtime create failed: %s (exit %d)", ci.State.Error, ci.State.ExitCode)
}

// claimedVolumes returns the names of the volumes still spoken for by a
// container this sweep left on the device, derived the same way create derived
// them. Containers in dropped are gone, so their volumes are collectable.
//
// A container that fabricates a volume but carries no image id cannot be
// turned into a volume name, which makes the claim set incomplete rather than
// short. It errors so the caller abandons the sweep: "no container claims this
// volume" and "we could not ask" must not collapse into one answer.
//
// A container whose removal failed and left it dead still claims here.
// mobynit's claim set, which extension-rollback and the initramfs read,
// excludes Dead and RemovalInProgress. The manager is deliberately the more
// conservative of the two: a volume retained one boot too long costs disk, and
// a volume collected under a container that comes back costs a failed deploy.
// Do not align them in the other direction.
func claimedVolumes(containers []Container, dropped map[string]bool) (map[string]bool, error) {
	claimed := make(map[string]bool)
	for _, c := range containers {
		if dropped[c.ID] || !labels.FabricatesVolume(c.Labels) {
			continue
		}
		if c.ImageID == "" {
			return nil, fmt.Errorf("the engine reported no image id for container %s, so its volume cannot be named",
				labels.ShortID(c.ID))
		}
		service, _ := labels.ResolveServiceName(c.Labels, c.ID)
		claimed[labels.VolumeName(service, c.ImageID)] = true
	}
	return claimed, nil
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

// runningKernelABIID returns the kernel identity the initrd published for
// the booted kernel: the balena_kernel_abi cmdline token, which carries the
// sha256 of the kernel image that was kexec'd. Returns "" with nil error
// when the token is absent (stock kernel boot): extensions that claim
// kernel-abi-id against such a device fail their claim naturally through the
// `stale` predicate.
func runningKernelABIID() (string, error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", fmt.Errorf("read /proc/cmdline: %w", err)
	}
	return hostapp.ParseHostKernelABIID(string(data)), nil
}
