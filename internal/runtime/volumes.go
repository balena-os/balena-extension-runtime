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
)

// fabricatingMarker names the file written into a volume during a fill. A kill
// mid-copy leaves a volume that is neither empty nor complete, and emptiness
// cannot tell that apart from a finished fill.
const fabricatingMarker = ".fabricating"

// bootDest is the only destination the runtime fabricates. It is structural,
// not configurable: mobynit's ABI gate hashes a regular file directly under
// the extension's /boot, boot-by-abi links into it, and the initramfs reads
// the kernel from it.
const bootDest = "/boot"

// createVolume is a test seam.
var createVolume = func(ctx context.Context, name string, volumeLabels map[string]string) (*manager.Volume, error) {
	return manager.NewEngine().CreateVolume(ctx, name, volumeLabels)
}

// withOperationLock is a test seam; the real lock lives in /run.
var withOperationLock = manager.WithOperationLock

// fabricateBootVolume creates and fills the /boot volume of a kernel override.
// It returns the volume's name, which create records and activate reads at
// start. A userspace-only extension gets "" and no engine call. The volume is
// never attached to the container.
//
// The lock covers create-or-get and the fill because create-or-get adopts an
// existing volume. Cleanup holds the lock across its container list and its
// volume removals, so without this a boot-time sweep can remove that volume
// between the adopt and start's publish, and activation then fails on a kernel
// that is not there. The lock need not extend over start: the container record
// is already in the store, so a sweep listing containers after this claims the
// volume.
func fabricateBootVolume(ctx context.Context, logger *slog.Logger, id oci.Identity, rootfs, containerID string) (string, error) {
	name, err := labels.BootVolume(id.Labels, containerID, id.ImageID)
	if err != nil || name == "" {
		return "", err
	}
	dataDir, err := oci.VolumeDataDir(name)
	if err != nil {
		return "", err
	}

	// The lock covers the adopt; see above.
	if err := withOperationLock(ctx, func() error {
		if _, err := createVolume(ctx, name, labels.Image(id.Labels)); err != nil {
			return fmt.Errorf("create volume %s: %w", name, err)
		}
		if err := fillVolume(logger, filepath.Join(rootfs, bootDest), dataDir); err != nil {
			return fmt.Errorf("fill volume %s: %w", name, err)
		}
		return nil
	}); err != nil {
		return "", err
	}
	logger.Info("fabricated extension volume", "volume", name, "dest", bootDest, "source", dataDir)
	return name, nil
}

// fillVolume seeds a volume from the extension's rootfs, mirroring the
// copy-on-create the engine performs for an image-declared volume. A volume
// that already holds content is left alone: it is a previous fill of the same
// image id. Nothing else ever writes here: activate publishes a symlink to the
// volume during start, and every other consumer reads.
//
// The flock serialises fills across processes. Two containers deployed from the
// same service and image share a volume, and the engine serialises creates per
// container rather than per volume: without it one fill can wipe the tree
// another is copying into, and both then finish believing the volume is
// complete. The lock is held on the volume directory itself, so it costs no
// file inside a directory whose emptiness is load-bearing.
func fillVolume(logger *slog.Logger, src, dataDir string) error {
	// One fill at a time, across processes.
	dir, err := os.Open(dataDir)
	if err != nil {
		// Not under the docker root: no boot-by-abi link.
		return fmt.Errorf("open volume data directory %s: %w", dataDir, err)
	}
	defer func() { _ = dir.Close() }()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", dataDir, err)
	}
	defer func() { _ = syscall.Flock(int(dir.Fd()), syscall.LOCK_UN) }()

	marker := filepath.Join(dataDir, fabricatingMarker)
	interrupted, err := exists(marker)
	if err != nil {
		return err
	}
	if interrupted {
		logger.Warn("volume holds a partial fill, wiping and refilling", "dir", dataDir)
		if err := wipe(dataDir); err != nil {
			return err
		}
	} else {
		empty, err := isEmpty(dataDir)
		if err != nil {
			return err
		}
		if !empty {
			logger.Debug("volume already filled, leaving it as it is", "dir", dataDir)
			return nil
		}
	}

	info, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// An absent /boot is an image defect. activate declines it.
			logger.Warn("the extension rootfs has no /boot, leaving the volume empty", "path", src)
			return nil
		}
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory in the extension rootfs", src)
	}
	// The marker and the copy share a directory.
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
	// The marker must be durable before the copy.
	if err := syncDir(dataDir); err != nil {
		return err
	}
	if err := copyTree(logger, src, dataDir); err != nil {
		return err
	}
	// copyTree synced the content, so the marker can go.
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("remove fabrication marker: %w", err)
	}
	return syncDir(dataDir)
}

// copyTree copies directories, regular files and symlinks from src into dst,
// and leaves what it wrote durable: copyFile syncs file data, and each
// directory is synced once filled, so a crash cannot lose the name of a file
// whose content survived. A link needs no sync of its own.
//
// Devices and sockets are skipped: they carry no content worth copying.
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
// yields a volume that passes every emptiness check with the name the
// initramfs loads missing.
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
// preserves the mode the image declared. An existing name is an error: the
// destination is empty or freshly wiped under the lock, so a collision means
// an assumption broke.
func mkdir(dir string, perm os.FileMode) error {
	if err := os.Mkdir(dir, perm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
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
	// The marker's guarantee needs the data on disk.
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	// Defeat the umask on the create above.
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
