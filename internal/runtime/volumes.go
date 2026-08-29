package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/manager"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// fabricatingMarker names the file written into a volume for the duration of
// a fill. A kill mid-copy leaves a volume that is neither empty nor complete,
// and emptiness alone cannot tell that apart from a finished fill; the marker
// is what makes the fill restartable.
const fabricatingMarker = ".fabricating"

// bootDest is the only destination the runtime fabricates. It is structural,
// not configurable: mobynit's ABI gate hashes a regular file directly under
// the extension's /boot, boot-by-abi links into it, and the initramfs reads
// the kernel from it.
const bootDest = "/boot"

// createVolume is a test seam, following the package-level function var
// convention used for the proxy.
var createVolume = func(ctx context.Context, name string, volumeLabels map[string]string) (*manager.Volume, error) {
	return manager.NewEngine().CreateVolume(ctx, name, volumeLabels)
}

// withOperationLock is a test seam; the real lock lives in /run.
var withOperationLock = manager.WithOperationLock

// fabricateBootVolume creates and fills the /boot volume of a kernel
// override, returning the host path that carries it into the hook
// environment. A userspace-only extension gets "" and no engine call.
//
// The volume is never attached to the container.
func fabricateBootVolume(ctx context.Context, logger *slog.Logger, spec *specs.Spec, stored oci.StoredConfig, rootfs, containerID string) (string, error) {
	identity := stored.Labels
	if len(identity) == 0 {
		identity = spec.Annotations
	}
	if !labels.FabricatesVolume(identity) {
		return "", nil
	}
	if stored.ImageID == "" {
		return "", fmt.Errorf("the container store gave no image id for %s, so the volume cannot be named", containerID)
	}

	service, fellBack := labels.ResolveServiceName(identity, containerID)
	if fellBack {
		// Worth a line: it makes the volume name unpredictable to anyone
		// reading it later.
		logger.Info("no service name label, naming the volume after the container id",
			"label", labels.ServiceName, "service", service)
	}
	name := labels.VolumeName(service, stored.ImageID)

	// Create-or-get adopts an existing volume, so without the lock cleanup
	// holds across its container list and its volume removals, a boot-time
	// sweep can remove that volume between the adopt and the create hook's
	// symlink, and the start hook then fails on a kernel that is not there.
	//
	// It need not extend over the create hook: the container record is already
	// in the store by the time the runtime is invoked, so a sweep listing
	// containers after this returns claims the volume.
	var mountpoint string
	if err := withOperationLock(ctx, func() error {
		volume, err := createVolume(ctx, name, labels.Image(identity))
		if err != nil {
			return fmt.Errorf("create volume %s: %w", name, err)
		}
		if volume.Mountpoint == "" {
			return fmt.Errorf("engine returned no mountpoint for volume %s", name)
		}
		if err := fillVolume(logger, filepath.Join(rootfs, bootDest), volume.Mountpoint); err != nil {
			return fmt.Errorf("fill volume %s: %w", name, err)
		}
		mountpoint = volume.Mountpoint
		return nil
	}); err != nil {
		return "", err
	}
	logger.Info("fabricated extension volume", "volume", name, "dest", bootDest, "source", mountpoint)

	return mountpoint, nil
}

// fillVolume seeds a volume from the extension's rootfs, mirroring the
// copy-on-create the engine performs for an image-declared volume.
//
// A volume that already holds content is left alone: it is a previous fill of
// the same image id. Nothing else ever writes here, the create hook publishes
// a symlink to the volume and every other consumer reads.
func fillVolume(logger *slog.Logger, src, mountpoint string) error {
	// One fill at a time, across processes. Two containers deployed from the
	// same service and image share a volume, and the engine serialises creates
	// per container rather than per volume: without this, one fill can wipe the
	// tree another is copying into and both then finish believing the volume is
	// complete. The lock is held on the volume directory itself so it costs no
	// file inside a directory whose emptiness is load-bearing.
	dir, err := os.Open(mountpoint)
	if err != nil {
		return fmt.Errorf("open %s: %w", mountpoint, err)
	}
	defer func() { _ = dir.Close() }()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", mountpoint, err)
	}
	defer func() { _ = syscall.Flock(int(dir.Fd()), syscall.LOCK_UN) }()

	marker := filepath.Join(mountpoint, fabricatingMarker)
	interrupted, err := exists(marker)
	if err != nil {
		return err
	}
	if interrupted {
		logger.Warn("volume holds a partial fill, wiping and refilling", "mountpoint", mountpoint)
		if err := wipe(mountpoint); err != nil {
			return err
		}
	} else {
		empty, err := isEmpty(mountpoint)
		if err != nil {
			return err
		}
		if !empty {
			logger.Debug("volume already filled, leaving it as it is", "mountpoint", mountpoint)
			return nil
		}
	}

	info, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The engine leaves the volume empty when the image has nothing
			// at that path, and so do we: the declaration is honoured, the
			// hooks still get a path to write into.
			logger.Warn("declared volume path is absent from the extension rootfs, leaving the volume empty", "path", src)
			return nil
		}
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory in the extension rootfs", src)
	}
	// The marker shares a directory with the copy, so an image shipping the
	// same name would either lose that file when the marker is removed or, as
	// a directory, wedge every retry on the same failed copy.
	collides, err := exists(filepath.Join(src, fabricatingMarker))
	if err != nil {
		return err
	}
	if collides {
		return fmt.Errorf("the extension ships %s, a name reserved for the fill marker",
			filepath.Join(bootDest, fabricatingMarker))
	}

	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return fmt.Errorf("write fabrication marker: %w", err)
	}
	// The marker has to reach disk before the copy it guards, or a crash can
	// leave a half-filled volume with nothing on disk to say so.
	if err := syncDir(mountpoint); err != nil {
		return err
	}
	if err := copyTree(logger, src, mountpoint); err != nil {
		return err
	}
	// copyTree has synced the content by here. Removing the marker before that
	// would let a crash persist the removal while the copied kernel was still
	// only in page cache, leaving a truncated volume that reads as complete
	// forever after.
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("remove fabrication marker: %w", err)
	}
	return syncDir(mountpoint)
}

// copyTree copies directories, regular files and symlinks from src into dst,
// and leaves what it wrote durable: file data is synced by copyFile, and each
// directory is synced once filled so a crash cannot lose the names of files
// whose content survived. A link needs no sync of its own, being nothing but
// an entry in the directory that sync covers.
//
// Devices and sockets are skipped: they carry no content worth copying, and a
// boot tree that ships one is not asking for it to be published.
func copyTree(logger *slog.Logger, src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", srcPath, err)
		}
		switch {
		case info.IsDir():
			if err := mkdir(dstPath, info.Mode().Perm()); err != nil {
				return err
			}
			if err := copyTree(logger, srcPath, dstPath); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFile(srcPath, dstPath, info.Mode().Perm()); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			if err := copyLink(srcPath, dstPath); err != nil {
				return err
			}
		default:
			logger.Warn("skipping non-regular file while filling volume",
				"path", srcPath, "mode", info.Mode().String())
		}
	}
	return syncDir(dst)
}

// copyLink reproduces a link rather than what it resolves to. A boot tree
// names its kernel through one (vmlinuz -> vmlinuz-<version>), and dropping it
// yields a volume that passes every emptiness check while the name the
// initramfs loads is missing.
func copyLink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("read link %s: %w", src, err)
	}
	if err := os.Symlink(target, dst); err != nil {
		return fmt.Errorf("link %s: %w", dst, err)
	}
	return nil
}

// mkdir creates dir with perm, defeating the process umask so the copy
// preserves the mode the image declared.
//
// The destination is empty or freshly wiped before the copy starts, and the
// lock keeps it that way, so a name that already exists means an assumption
// broke and the fill should say so rather than merge into whatever is there.
func mkdir(dir string, perm os.FileMode) error {
	if err := os.Mkdir(dir, perm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// The umask applies to the create above, so the mode is set explicitly.
	if err := os.Chmod(dir, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	return nil
}

// copyFile copies a regular file. O_NOFOLLOW closes the window between the
// caller's stat and this open, in which the source could have become a link.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	// The fill marker is only removed once the content it guards is durable,
	// so the data has to be on disk and not merely written.
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	// The umask applies to the create above, so the mode is set explicitly.
	if err := os.Chmod(dst, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}

// syncDir persists a directory's entries, which is separate from persisting
// the contents of the files they name.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// wipe empties dir without removing it: the directory itself belongs to the
// engine, which created it as the volume's data root.
func wipe(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("wipe %s: %w", dir, err)
		}
	}
	return nil
}

func isEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	return len(entries) == 0, nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

// hookMounts returns the bundle's mounts with the volume create fabricated
// merged in. start and delete re-read the spec from the bundle, which never
// carried that mount, so without the merge their hooks would run without the
// EXTENSION_VOLUME_BOOT variable the create hook was given.
//
// A missing or unreadable record degrades to the bundle's mounts alone rather
// than failing the call: losing an environment variable is recoverable, and
// refusing to delete a container is not.
func hookMounts(logger *slog.Logger, containerID string, specMounts []specs.Mount) []specs.Mount {
	source, err := oci.ReadBootVolume(containerID)
	if err != nil {
		logger.Warn("could not read the fabricated volume record, hooks run without it",
			"id", containerID, "err", err)
	}
	return withBootVolume(specMounts, source)
}

// withBootVolume overlays the fabricated /boot mount onto the bundle's,
// replacing anything the bundle declares at that destination. The runtime owns
// /boot for a kernel override, so the hook environment carries exactly one
// EXTENSION_VOLUME_BOOT entry rather than two the consumer would have to
// choose between. An empty source leaves the bundle's mounts as they are.
func withBootVolume(specMounts []specs.Mount, source string) []specs.Mount {
	if source == "" {
		return specMounts
	}
	merged := make([]specs.Mount, 0, len(specMounts)+1)
	for _, m := range specMounts {
		if filepath.Clean(m.Destination) != bootDest {
			merged = append(merged, m)
		}
	}
	return append(merged, specs.Mount{Destination: bootDest, Type: "volume", Source: source})
}
