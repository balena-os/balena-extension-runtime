package runtime

import (
	"bufio"
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
	"github.com/balena-os/hostapp"
)

// The host paths activation touches, with internal/bootenv the whole host
// contract. Variables so tests can redirect them.
var (
	// One link per published kernel, read by the initramfs.
	bootByABIDir = "/mnt/data/boot-by-abi"

	// Carries the validator's records across a boot.
	stateMount = "/mnt/state"

	// The VPN reachability the validator compares against. openvpn's
	// upscript and rollback-tests both spell it /run.
	vpnActiveMarker = "/run/openvpn/vpn_status/active"
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
// kernel activates nothing.
//
// A hooks.ErrRejected is the extension's verdict and no retry changes it.
// Any other error is a machine condition the caller can retry.
//
// The step order below is the safety argument; nothing else enforces it.
func activate(logger *slog.Logger, containerID, rootfs string, annotations map[string]string) error {
	if !labels.FabricatesVolume(annotations) {
		return nil
	}
	abi := annotations[labels.KernelABIID]

	mounted, err := isMounted(stateMount)
	if err != nil {
		return fmt.Errorf("checking whether %s is mounted: %w", stateMount, err)
	}
	if !mounted {
		return fmt.Errorf("%s is not mounted, refusing to arm %s", stateMount, abi)
	}

	listed, err := rejectedABI(abi)
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

	if err := publishKernel(abi, target); err != nil {
		return err
	}
	if err := writeHealthPrestate(); err != nil {
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

// rejectedABI reports whether boot-time validation already rejected this
// kernel. An absent record is empty; an unreadable one is a machine condition.
func rejectedABI(abi string) (bool, error) {
	path := filepath.Join(stateMount, "override-rejected")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if scanner.Text() == abi {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
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

// publishKernel points boot-by-abi/<abi> at the kernel image itself, so that
// the link resolving is the same question as the kernel being there.
// A republish overwrites, because a retry is a recreate.
func publishKernel(abi, target string) error {
	if err := os.MkdirAll(bootByABIDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", bootByABIDir, err)
	}
	tmp := filepath.Join(bootByABIDir, abi+".new")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear %s: %w", tmp, err)
	}
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("link %s: %w", tmp, err)
	}
	link := filepath.Join(bootByABIDir, abi)
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish %s: %w", link, err)
	}
	// Durable before anything else names the ABI.
	return syncDir(bootByABIDir)
}

// writeHealthPrestate records VPN reachability for the next boot's validator.
//
// Written through a temporary name: extension-rollback removes this file when
// it closes a window, so its presence is what says a window is open. A
// truncating write would leave an open window reading an empty prestate.
func writeHealthPrestate() error {
	value := "BALENAOS_ROLLBACK_VPNONLINE=0\n"
	if _, err := os.Stat(vpnActiveMarker); err == nil {
		value = "BALENAOS_ROLLBACK_VPNONLINE=1\n"
	}
	path := filepath.Join(stateMount, "extension-health-variables")
	tmp := path + ".new"
	if err := writeFileSynced(tmp, value); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish %s: %w", path, err)
	}
	// A new name needs its directory entry on disk.
	return syncDir(stateMount)
}

// writeFileSynced writes content to path and fsyncs it before returning.
func writeFileSynced(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
