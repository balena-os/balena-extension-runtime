package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/manager"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain neutralises the cross-process operation lock: its real home is /run,
// which a test run cannot write to. The lock itself is proven in
// internal/manager; this package only pins that fabrication takes it.
func TestMain(m *testing.M) {
	withOperationLock = func(_ context.Context, fn func() error) error { return fn() }
	os.Exit(m.Run())
}

// defaultDockerRoot is the production default. A test that redirects the
// docker root restores it.
const defaultDockerRoot = "/var/lib/docker"

// stubEngine stands in for the engine's volume layout. It records the volume
// fabrication asks for, and creates that volume's data directory under a
// temporary docker root.
type stubEngine struct {
	root    string
	created map[string]map[string]string
	order   []string
	err     error
}

func newStubEngine(t *testing.T) *stubEngine {
	t.Helper()
	s := &stubEngine{root: t.TempDir(), created: map[string]map[string]string{}}
	oci.SetDockerRoot(s.root)
	prev := createVolume
	createVolume = func(_ context.Context, name string, volumeLabels map[string]string) (*manager.Volume, error) {
		if s.err != nil {
			return nil, s.err
		}
		dataDir := s.dataDir(name)
		if _, seen := s.created[name]; !seen {
			// Create-or-get: only the first call records labels.
			s.created[name] = volumeLabels
			s.order = append(s.order, name)
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				return nil, err
			}
		}
		return &manager.Volume{Name: name, Labels: s.created[name]}, nil
	}
	t.Cleanup(func() {
		createVolume = prev
		oci.SetDockerRoot(defaultDockerRoot)
	})
	return s
}

func (s *stubEngine) dataDir(name string) string {
	return filepath.Join(s.root, "volumes", name, "_data")
}

// extensionRootfs builds a rootfs holding /boot with a kernel file, a nested
// directory, and the two symlink shapes a boot tree produces: the ordinary
// relative alias, and an absolute one that must be copied as a link rather
// than followed out of the rootfs.
func extensionRootfs(t *testing.T) string {
	t.Helper()
	rootfs := t.TempDir()
	boot := filepath.Join(rootfs, "boot")
	require.NoError(t, os.MkdirAll(filepath.Join(boot, "dtb"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(boot, "kernel"), []byte("vmlinuz"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(boot, "dtb", "board.dtb"), []byte("fdt"), 0o600))
	require.NoError(t, os.Symlink("kernel", filepath.Join(boot, "vmlinuz")))
	require.NoError(t, os.Symlink("/etc/shadow", filepath.Join(boot, "escape")))
	return rootfs
}

// kernelOverride is the annotation set of an extension carrying a kernel. The
// ABI id is what admits it to fabrication.
func kernelOverride(extra ...string) map[string]string {
	annotations := map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-integration",
		"io.balena.service-name":        "kernel-modules",
	}
	for i := 0; i < len(extra); i += 2 {
		annotations[extra[i]] = extra[i+1]
	}
	return annotations
}

// identity is what create resolves before fabrication. A read of the container
// store yields the same shape.
func identity(lbls map[string]string, imageID string) oci.Identity {
	return oci.Identity{Labels: lbls, ImageID: imageID}
}

func TestFabricateBootVolume_FillsFromRootfs(t *testing.T) {
	stub := newStubEngine(t)
	rootfs := extensionRootfs(t)

	name, err := fabricateBootVolume(context.Background(), testLogger(),
		identity(kernelOverride("maintainer", "someone"), "sha256:42befc76f4f8aaaa"), rootfs, "0123456789abcdef")
	require.NoError(t, err)

	require.Equal(t, []string{name}, stub.order, "exactly one volume, backing /boot")

	// Cleanup and the supervisor read only the class label.
	// The other image labels are for operators.
	assert.Equal(t, map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-integration",
	}, stub.created[name])

	kernel, err := os.ReadFile(filepath.Join(stub.dataDir(name), "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "vmlinuz", string(kernel))

	dtb, err := os.ReadFile(filepath.Join(stub.dataDir(name), "dtb", "board.dtb"))
	require.NoError(t, err)
	assert.Equal(t, "fdt", string(dtb))

	info, err := os.Stat(filepath.Join(stub.dataDir(name), "dtb", "board.dtb"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "mode must be preserved")

	target, err := os.Readlink(filepath.Join(stub.dataDir(name), "vmlinuz"))
	require.NoError(t, err)
	assert.Equal(t, "kernel", target, "a boot tree's kernel alias must survive the copy")

	assert.NoFileExists(t, filepath.Join(stub.dataDir(name), fabricatingMarker),
		"the marker must be gone once the fill completes")
}

// TestFabricateBootVolume_ReturnsTheName pins what create hands to start: the
// volume's name. No path crosses that record, because activate rebuilds every
// path from the name.
func TestFabricateBootVolume_ReturnsTheName(t *testing.T) {
	newStubEngine(t)

	name, err := fabricateBootVolume(context.Background(), testLogger(),
		identity(kernelOverride(), "sha256:42befc76f4f8aaaa"), extensionRootfs(t), "0123456789abcdef")
	require.NoError(t, err)

	assert.Equal(t, "ext_kernel-modules_42befc76f4f8_boot", name)
}

// TestFabricateBootVolume_VolumeOutsideTheDockerRoot covers an engine whose
// volume layout is not the one this OS was built against. The fill only reads
// the path under the docker root, so it fails there and copies nothing.
func TestFabricateBootVolume_VolumeOutsideTheDockerRoot(t *testing.T) {
	stub := newStubEngine(t)
	elsewhere := t.TempDir()
	prev := createVolume
	createVolume = func(_ context.Context, name string, volumeLabels map[string]string) (*manager.Volume, error) {
		dataDir := filepath.Join(elsewhere, "volumes", name, "_data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, err
		}
		return &manager.Volume{Name: name, Labels: volumeLabels}, nil
	}
	t.Cleanup(func() { createVolume = prev })

	_, err := fabricateBootVolume(context.Background(), testLogger(),
		identity(kernelOverride(), "sha256:42befc76f4f8aaaa"), extensionRootfs(t), "abc")
	require.Error(t, err)

	const name = "ext_kernel-modules_42befc76f4f8_boot"
	assert.Contains(t, err.Error(), stub.dataDir(name))
	empty, err := isEmpty(filepath.Join(elsewhere, "volumes", name, "_data"))
	require.NoError(t, err)
	assert.True(t, empty, "a volume the docker root does not cover must not be filled")
}

// TestFillVolume_CopiesSymlinksWithoutFollowing pins the copier against the
// mistake that reads as a complete volume and boots nothing. A boot tree names
// its kernel through a link, so dropping links produces a volume that passes
// every emptiness check while the name the initramfs loads is absent.
//
// The link is reproduced verbatim, never resolved: following an absolute
// target would copy host content into the volume under a name the extension
// chose, and following a relative one would turn a hardlink-cheap alias into a
// second copy of the kernel.
func TestFillVolume_CopiesSymlinksWithoutFollowing(t *testing.T) {
	dataDir := t.TempDir()
	src := filepath.Join(extensionRootfs(t), "boot")
	require.NoError(t, os.Symlink("initrd-6.6.20", filepath.Join(src, "initrd")))

	require.NoError(t, fillVolume(testLogger(), src, dataDir))

	for _, tc := range []struct {
		name, link, target string
	}{
		{"relative alias", "vmlinuz", "kernel"},
		{"absolute target outside the rootfs", "escape", "/etc/shadow"},
		{"dangling target", "initrd", "initrd-6.6.20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dataDir, tc.link)
			info, err := os.Lstat(path)
			require.NoError(t, err)
			require.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
				"the entry must be a link, not the content it resolves to")

			target, err := os.Readlink(path)
			require.NoError(t, err)
			assert.Equal(t, tc.target, target)
		})
	}
}

// TestFillVolume_SkipsNonRegularFiles keeps the copier's remaining exclusion
// honest: a device or a socket has no meaning once copied, so it is stepped
// past rather than turned into an error that fails the create.
func TestFillVolume_SkipsNonRegularFiles(t *testing.T) {
	dataDir := t.TempDir()
	src := filepath.Join(extensionRootfs(t), "boot")
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644))

	require.NoError(t, fillVolume(testLogger(), src, dataDir))

	_, err := os.Lstat(filepath.Join(dataDir, "pipe"))
	assert.True(t, errors.Is(err, os.ErrNotExist), "a fifo must not be recreated")
	assert.FileExists(t, filepath.Join(dataDir, "kernel"), "the rest of the tree still copies")
}

// TestFabricateBootVolume_NoKernelABIID pins the admission rule: a
// userspace-only extension declares no ABI, so it gets no volume and the
// engine is never called.
func TestFabricateBootVolume_NoKernelABIID(t *testing.T) {
	stub := newStubEngine(t)
	id := identity(map[string]string{
		"io.balena.image.class":  "overlay",
		"io.balena.service-name": "userspace-only",
	}, "sha256:42befc76f4f8aaaa")

	name, err := fabricateBootVolume(context.Background(), testLogger(), id, extensionRootfs(t), "0123456789abcdef")
	require.NoError(t, err)

	assert.Empty(t, name)
	assert.Empty(t, stub.order, "an extension without a kernel must not reach the engine")
}

// TestFabricateBootVolume_ServiceNameFallback covers a manual deploy, where no
// service label exists and the container id has to name the volume.
func TestFabricateBootVolume_ServiceNameFallback(t *testing.T) {
	stub := newStubEngine(t)
	annotations := kernelOverride()
	delete(annotations, "io.balena.service-name")

	_, err := fabricateBootVolume(context.Background(), testLogger(), identity(annotations, "sha256:42befc76f4f8aaaa"), extensionRootfs(t), "0123456789abcdeffedcba")
	require.NoError(t, err)

	assert.Equal(t, []string{"ext_0123456789ab_42befc76f4f8_boot"}, stub.order)
}

// TestFabricateBootVolume_Idempotent asserts a second create reuses the volume
// and leaves its contents alone: a filled volume is a previous fill of the
// same image id, and rewriting it would truncate a kernel boot-by-abi may
// already link to.
func TestFabricateBootVolume_Idempotent(t *testing.T) {
	stub := newStubEngine(t)
	rootfs := extensionRootfs(t)
	id := identity(kernelOverride(), "sha256:42befc76f4f8aaaa")

	_, err := fabricateBootVolume(context.Background(), testLogger(), id, rootfs, "abc")
	require.NoError(t, err)

	name := "ext_kernel-modules_42befc76f4f8_boot"
	written := filepath.Join(stub.dataDir(name), "written-after-fill")
	require.NoError(t, os.WriteFile(written, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stub.dataDir(name), "kernel"), []byte("patched"), 0o644))

	_, err = fabricateBootVolume(context.Background(), testLogger(), id, rootfs, "abc")
	require.NoError(t, err)

	assert.Equal(t, []string{name}, stub.order, "the same image must key the same volume")
	assert.FileExists(t, written, "a filled volume must be left alone")
	kernel, err := os.ReadFile(filepath.Join(stub.dataDir(name), "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "patched", string(kernel), "content must not be re-copied over")
}

// TestFabricateBootVolume_MarkerRecovery covers a kill mid-copy: the volume is
// neither empty nor complete, so emptiness alone would skip the refill
// forever. The marker is what makes the partial state recognisable.
func TestFabricateBootVolume_MarkerRecovery(t *testing.T) {
	stub := newStubEngine(t)
	rootfs := extensionRootfs(t)
	id := identity(kernelOverride(), "sha256:42befc76f4f8aaaa")

	name := "ext_kernel-modules_42befc76f4f8_boot"
	dataDir := stub.dataDir(name)
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, fabricatingMarker), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "kernel"), []byte("trunc"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "leftover"), []byte("junk"), 0o644))

	_, err := fabricateBootVolume(context.Background(), testLogger(), id, rootfs, "abc")
	require.NoError(t, err)

	kernel, err := os.ReadFile(filepath.Join(dataDir, "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "vmlinuz", string(kernel), "a partial fill must be refilled from the rootfs")
	assert.NoFileExists(t, filepath.Join(dataDir, "leftover"),
		"the wipe must clear what the interrupted copy left behind")
	assert.NoFileExists(t, filepath.Join(dataDir, fabricatingMarker))
}

// TestFabricateBootVolume_MissingRootfsPath asserts a kernel override whose
// rootfs carries no /boot leaves an empty volume rather than failing the
// create, which is what the engine's own copy-on-create does.
func TestFabricateBootVolume_MissingRootfsPath(t *testing.T) {
	stub := newStubEngine(t)

	name, err := fabricateBootVolume(context.Background(), testLogger(), identity(kernelOverride(), "sha256:42befc76f4f8aaaa"), t.TempDir(), "abc")
	require.NoError(t, err)

	require.NotEmpty(t, name)
	empty, err := isEmpty(stub.dataDir("ext_kernel-modules_42befc76f4f8_boot"))
	require.NoError(t, err)
	assert.True(t, empty)
}

// TestFabricateBootVolume_MarkerNameCollision covers an image that ships the
// name the fill marker reserves. Copying it in would either lose the file when
// the marker is removed or, as a directory, wedge every retry, so the create
// fails with the collision named instead.
func TestFabricateBootVolume_MarkerNameCollision(t *testing.T) {
	newStubEngine(t)
	rootfs := extensionRootfs(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(rootfs, "boot", fabricatingMarker), []byte("mine"), 0o644))

	_, err := fabricateBootVolume(context.Background(), testLogger(), identity(kernelOverride(), "sha256:42befc76f4f8aaaa"), rootfs, "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), fabricatingMarker)
}

// TestFillVolume_ExcludesAConcurrentFill covers two containers deployed from
// the same service and image: they share one volume, and the engine serialises
// creates per container rather than per volume. Without exclusion one fill
// wipes the tree the other is copying into and both then finish believing the
// volume is complete.
func TestFillVolume_ExcludesAConcurrentFill(t *testing.T) {
	dataDir := t.TempDir()
	src := filepath.Join(extensionRootfs(t), "boot")

	// Stand in for the other create's fill.
	holder, err := os.Open(dataDir)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX))

	done := make(chan error, 1)
	go func() { done <- fillVolume(testLogger(), src, dataDir) }()

	select {
	case <-done:
		t.Fatal("a fill proceeded while another process held the volume")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_UN))
	require.NoError(t, holder.Close())
	require.NoError(t, <-done, "the fill must proceed once the volume is released")

	kernel, err := os.ReadFile(filepath.Join(dataDir, "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "vmlinuz", string(kernel))
}

func TestFabricateBootVolume_EngineFailure(t *testing.T) {
	stub := newStubEngine(t)
	stub.err = errors.New("engine unavailable")

	_, err := fabricateBootVolume(context.Background(), testLogger(), identity(kernelOverride(), "sha256:42befc76f4f8aaaa"), extensionRootfs(t), "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "engine unavailable")
}

// TestFabricateBootVolume_NoImageID asserts fabrication refuses to name a
// volume it cannot key on the image, rather than producing a name that
// collides across builds.
func TestFabricateBootVolume_NoImageID(t *testing.T) {
	newStubEngine(t)

	_, err := fabricateBootVolume(context.Background(), testLogger(), identity(kernelOverride(), ""), extensionRootfs(t), "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image id")
}

// Create-or-get and the fill both happen inside the manager's operation lock.
func TestFabricateBootVolume_FillsUnderTheOperationLock(t *testing.T) {
	stub := newStubEngine(t)
	rootfs := extensionRootfs(t)
	const name = "ext_kernel-modules_42befc76f4f8_boot"

	var createdInside, filledOnRelease bool
	held := false

	prevLock := withOperationLock
	withOperationLock = func(ctx context.Context, fn func() error) error {
		held = true
		err := fn()
		// A fill outside the lock would not have run yet.
		_, statErr := os.Stat(filepath.Join(stub.dataDir(name), "kernel"))
		filledOnRelease = statErr == nil
		held = false
		return err
	}
	t.Cleanup(func() { withOperationLock = prevLock })

	prevCreate := createVolume
	createVolume = func(ctx context.Context, n string, l map[string]string) (*manager.Volume, error) {
		createdInside = held
		return prevCreate(ctx, n, l)
	}
	t.Cleanup(func() { createVolume = prevCreate })

	_, err := fabricateBootVolume(context.Background(), testLogger(),
		identity(kernelOverride(), "sha256:42befc76f4f8aaaa"), rootfs, "0123456789abcdef")
	require.NoError(t, err)

	assert.True(t, createdInside, "create-or-get must run under the operation lock")
	assert.True(t, filledOnRelease, "the fill must finish before the lock is released")
}
