package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/mounts"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/override"
	"github.com/balena-os/hostapp"
)

// Test seams.
var (
	isMounted   = mounts.IsMounted
	armOverride = bootenv.Arm
)

// errVerdict marks an image defect that no retry can fix.
var errVerdict = errors.New("extension verdict")

// activate installs and arms a kernel override. An extension carrying no
// kernel activates nothing. Any error other than errVerdict is a machine
// condition.
//
// Every check precedes the writes, and the writes keep their order.
// That is the safety argument, and nothing else enforces it. Image
// checks run first, so an image defect is a verdict on every machine.
func activate(ctx context.Context, logger *slog.Logger, containerID, rootfs string, annotations map[string]string) error {
	if !labels.FabricatesVolume(annotations) {
		return nil
	}
	abi := annotations[labels.KernelABIID]

	// The label is a claim; these bytes verify it.
	image, skipped, err := hostapp.KernelImageForABIID(filepath.Join(rootfs, bootDest), abi)
	for _, s := range skipped {
		logger.Warn("skipping unreadable file while matching the kernel",
			"id", containerID, "err", s)
	}
	if err != nil {
		// Only an absent /boot is the image's fault.
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s: %s", errVerdict, labels.KernelABIID, err)
		}
		return fmt.Errorf("match %s against %s: %w", bootDest, labels.KernelABIID, err)
	}
	if image == "" {
		return fmt.Errorf("%w: no regular file under %s hashes to %s=%s",
			errVerdict, bootDest, labels.KernelABIID, abi)
	}

	// Only this can check the pairing: mobynit cannot mount.
	release, err := moduleRelease(rootfs)
	if err != nil {
		return err
	}
	if release == "" {
		return fmt.Errorf("%w: %s=%s claims a kernel the image ships no modules for",
			errVerdict, labels.KernelABIID, abi)
	}

	// A mismatched label boots without the extension's modules.
	if kver := annotations[labels.KernelVersion]; kver != "" && kver != kernelVersion(release) {
		return fmt.Errorf("%w: %s=%s does not match the %s modules the image ships",
			errVerdict, labels.KernelVersion, kver, release)
	}

	// Machine checks from here, except a listed ABI.
	mounted, err := isMounted(override.StateMount)
	if err != nil {
		return fmt.Errorf("checking whether %s is mounted: %w", override.StateMount, err)
	}
	if !mounted {
		return fmt.Errorf("%s is not mounted, refusing to arm %s", override.StateMount, abi)
	}

	listed, err := override.RejectedABI(abi)
	if err != nil {
		return err
	}
	if listed {
		return fmt.Errorf("%w: kernel override %s was rejected by health validation", errVerdict, abi)
	}

	name, err := oci.ReadBootVolume(containerID)
	if err != nil {
		return err
	}
	source, err := oci.VolumeDataDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("fabricated volume for %s: %w", containerID, err)
	}
	// A volume without the kernel would dangle the link.
	kernel := filepath.Base(image)
	if _, err := os.Stat(filepath.Join(source, kernel)); err != nil {
		return fmt.Errorf("fabricated volume for %s does not hold %s: %w", containerID, kernel, err)
	}
	rel, err := oci.VolumeRelDir(name)
	if err != nil {
		return err
	}
	target, err := override.KernelTarget(rel, kernel)
	if err != nil {
		return err
	}

	// Every other writer of these records holds this lock.
	if err := withOperationLock(ctx, func() error {
		if err := override.PublishKernel(abi, target); err != nil {
			return err
		}
		if err := override.WriteHealthPrestate(); err != nil {
			return err
		}
		// The hash match above proves abi well formed.
		if err := armOverride(abi); err != nil {
			return fmt.Errorf("arm kernel override %s: %w", abi, err)
		}
		return nil
	}); err != nil {
		return err
	}

	logger.Info("activated kernel override", "id", containerID, "abi", abi, "kernel", image, "target", target)
	return nil
}

// The one path mobynit claims and binds through. A merged-usr rootfs
// reaches it through a relative lib link.
const modulesDir = "lib/modules"

// The release directory holding Module.symvers, the signal the build pairs
// the kernel-abi-id label with. Empty when the image ships none. A modules or
// release directory that resolves outside its parent is a verdict: mobynit
// would not bind it, and an absolute link reads this host, not the image.
func moduleRelease(rootfs string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(rootfs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", rootfs, err)
	}
	base := filepath.Join(rootfs, modulesDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", base, err)
	}
	modules, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", base, err)
	}
	if !strictlyUnder(realRoot, modules) {
		return "", fmt.Errorf("%w: %s resolves outside the image", errVerdict, modulesDir)
	}
	for _, e := range entries {
		release, err := filepath.EvalSymlinks(filepath.Join(modules, e.Name()))
		if err != nil {
			continue
		}
		if !strictlyUnder(modules, release) {
			return "", fmt.Errorf("%w: %s/%s resolves outside %s", errVerdict, modulesDir, e.Name(), modulesDir)
		}
		info, err := os.Stat(filepath.Join(release, "Module.symvers"))
		if err == nil && info.Mode().IsRegular() {
			return e.Name(), nil
		}
	}
	return "", nil
}

func strictlyUnder(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// The M.m.p the kernel-version label carries, with the release's
// local-version suffix stripped. Mirrors the filter mobynit applies.
func kernelVersion(release string) string {
	if i := strings.IndexByte(release, '-'); i > 0 {
		return release[:i]
	}
	return release
}
