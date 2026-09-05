package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/balena-os/balena-extension-runtime/internal/hooks"
	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/mounts"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/override"
	"github.com/balena-os/hostapp"
)

// Where the data partition carries the engine's volumes.
const dataVolumes = "docker/volumes"

// Test seams, following the package's convention for the proxy and the
// engine.
var (
	isMounted   = mounts.IsMounted
	armOverride = bootenv.Arm
)

// activate installs and arms a kernel override. An extension carrying no
// kernel activates nothing, and a hooks.ErrRejected is the extension's
// verdict rather than a machine condition.
//
// The step order below is the safety argument; nothing else enforces it.
func activate(logger *slog.Logger, containerID, rootfs string, annotations map[string]string) error {
	if !labels.FabricatesVolume(annotations) {
		return nil
	}
	abi := annotations[labels.KernelABIID]

	mounted, err := isMounted(override.StateMount)
	if err != nil {
		return fmt.Errorf("checking whether %s is mounted: %w", override.StateMount, err)
	}
	if !mounted {
		return fmt.Errorf("%s is not mounted, refusing to arm %s", override.StateMount, abi)
	}

	// A plain error from here is a machine condition; the record itself is
	// the extension's verdict.
	listed, err := override.RejectedABI(abi)
	if err != nil {
		return err
	}
	if listed {
		return fmt.Errorf("%w: kernel override %s was rejected by health validation", hooks.ErrRejected, abi)
	}

	// The label is a claim; these bytes verify it.
	image, skipped, err := hostapp.KernelImageForABIID(filepath.Join(rootfs, bootDest), abi)
	for _, s := range skipped {
		logger.Warn("skipping unreadable file while matching the kernel",
			"id", containerID, "err", s)
	}
	if err != nil {
		// Only an absent /boot is the image's fault.
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s: %s", hooks.ErrRejected, labels.KernelABIID, err)
		}
		return fmt.Errorf("match %s against %s: %w", bootDest, labels.KernelABIID, err)
	}
	if image == "" {
		return fmt.Errorf("%w: no regular file under %s hashes to %s=%s",
			hooks.ErrRejected, bootDest, labels.KernelABIID, abi)
	}

	// mobynit's claim query cannot mount, so only this can check the pairing.
	modules, err := hasModuleSymvers(rootfs)
	if err != nil {
		return err
	}
	if !modules {
		return fmt.Errorf("%w: %s=%s claims a kernel the image ships no modules for",
			hooks.ErrRejected, labels.KernelABIID, abi)
	}

	source, err := oci.ReadBootVolume(containerID)
	if err != nil {
		return fmt.Errorf("read the fabricated volume record for %s: %w", containerID, err)
	}
	if source == "" {
		return fmt.Errorf("no fabricated volume recorded for %s, nothing to publish", containerID)
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("fabricated volume for %s: %w", containerID, err)
	}
	// The link names the kernel, so a volume without it would dangle. create
	// fills the volume, so a gap here is not the image's fault.
	kernel := filepath.Base(image)
	if _, err := os.Stat(filepath.Join(source, kernel)); err != nil {
		return fmt.Errorf("fabricated volume for %s does not hold %s: %w", containerID, kernel, err)
	}
	target, err := volumeTarget(source, kernel)
	if err != nil {
		return err
	}

	if err := override.PublishKernel(abi, target); err != nil {
		return err
	}
	if err := override.WriteHealthPrestate(); err != nil {
		return err
	}
	if err := armOverride(abi); err != nil {
		return fmt.Errorf("arm kernel override %s: %w", abi, err)
	}

	logger.Info("activated kernel override", "id", containerID, "abi", abi, "kernel", image, "target", target)
	return nil
}

// Where a kernel override carries its drivers. Both spellings, since /lib is
// a symlink to /usr/lib only on a merged-usr rootfs.
var modulesDirs = []string{"usr/lib/modules", "lib/modules"}

// The signal the build pairs the kernel-abi-id label with.
func hasModuleSymvers(rootfs string) (bool, error) {
	for _, dir := range modulesDirs {
		base := filepath.Join(rootfs, dir)
		entries, err := os.ReadDir(base)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, fmt.Errorf("read %s: %w", base, err)
		}
		for _, e := range entries {
			// Stat: a release directory may itself be a symlink.
			info, err := os.Stat(filepath.Join(base, e.Name(), "Module.symvers"))
			if err == nil && info.Mode().IsRegular() {
				return true, nil
			}
		}
	}
	return false, nil
}

// volumeTarget returns the link target for a fabricated volume's kernel,
// relative to the data partition.
//
// Only the volume's name carries over. /var/lib/docker is a bind of
// /mnt/data/docker, so a path derived from the reported mountpoint crosses
// the bind and resolves in the running OS but not in the initramfs.
func volumeTarget(mountpoint, kernel string) (string, error) {
	clean := filepath.Clean(mountpoint)
	dir := filepath.Dir(clean)
	name := filepath.Base(dir)
	if filepath.Base(clean) != "_data" || filepath.Base(filepath.Dir(dir)) != "volumes" ||
		name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("fabricated volume mountpoint %q is not .../volumes/<name>/_data", mountpoint)
	}
	if kernel == "" || strings.ContainsRune(kernel, filepath.Separator) {
		return "", fmt.Errorf("kernel image name %q is not a bare file name", kernel)
	}
	return filepath.Join("..", dataVolumes, name, "_data", kernel), nil
}
