package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/manager"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
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

// stubEngine records the volume fabrication asks for and hands back a
// mountpoint under a temporary directory, standing in for the engine's own
// volume root.
type stubEngine struct {
	root    string
	created map[string]map[string]string
	order   []string
	err     error
}

func newStubEngine(t *testing.T) *stubEngine {
	t.Helper()
	s := &stubEngine{root: t.TempDir(), created: map[string]map[string]string{}}
	prev := createVolume
	createVolume = func(_ context.Context, name string, volumeLabels map[string]string) (*manager.Volume, error) {
		if s.err != nil {
			return nil, s.err
		}
		mountpoint := filepath.Join(s.root, name, "_data")
		if _, seen := s.created[name]; !seen {
			// Create-or-get: only the first call records labels, mirroring
			// the engine ignoring them for an existing volume.
			s.created[name] = volumeLabels
			s.order = append(s.order, name)
			if err := os.MkdirAll(mountpoint, 0o755); err != nil {
				return nil, err
			}
		}
		return &manager.Volume{Name: name, Mountpoint: mountpoint, Labels: s.created[name]}, nil
	}
	t.Cleanup(func() { createVolume = prev })
	return s
}

func (s *stubEngine) mountpoint(name string) string {
	return filepath.Join(s.root, name, "_data")
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

func specWith(annotations map[string]string, mounts ...specs.Mount) *specs.Spec {
	return &specs.Spec{Annotations: annotations, Mounts: mounts}
}

// stored is what the container store contributes: an image id and no labels,
// which is the shape a synthetic bundle produces. Identity then falls back to
// the spec's annotations, so these tests exercise that path. The case where
// the store does have labels is covered by
// TestFabricateBootVolume_StoreLabelsWinOverAnnotations.
func stored(imageID string) oci.StoredConfig {
	return oci.StoredConfig{ImageID: imageID}
}

func TestFabricateBootVolume_FillsFromRootfs(t *testing.T) {
	stub := newStubEngine(t)
	rootfs := extensionRootfs(t)
	spec := specWith(kernelOverride("maintainer", "someone"))

	source, err := fabricateBootVolume(context.Background(), testLogger(), spec, stored("sha256:42befc76f4f8aaaa"), rootfs, "0123456789abcdef")
	require.NoError(t, err)

	name := "ext_kernel-modules_42befc76f4f8_boot"
	require.Equal(t, []string{name}, stub.order, "exactly one volume, backing /boot")
	assert.Equal(t, stub.mountpoint(name), source)

	// The volume carries the image labels the commit sweep applies its
	// staleness predicate to, and nothing else: the sweep reaches it by
	// re-deriving its name, so no bookkeeping label has to survive on it.
	assert.Equal(t, map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-integration",
	}, stub.created[name])

	kernel, err := os.ReadFile(filepath.Join(stub.mountpoint(name), "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "vmlinuz", string(kernel))

	dtb, err := os.ReadFile(filepath.Join(stub.mountpoint(name), "dtb", "board.dtb"))
	require.NoError(t, err)
	assert.Equal(t, "fdt", string(dtb))

	info, err := os.Stat(filepath.Join(stub.mountpoint(name), "dtb", "board.dtb"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "mode must be preserved")

	target, err := os.Readlink(filepath.Join(stub.mountpoint(name), "vmlinuz"))
	require.NoError(t, err)
	assert.Equal(t, "kernel", target, "a boot tree's kernel alias must survive the copy")

	assert.NoFileExists(t, filepath.Join(stub.mountpoint(name), fabricatingMarker),
		"the marker must be gone once the fill completes")
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
	mountpoint := t.TempDir()
	src := filepath.Join(extensionRootfs(t), "boot")
	require.NoError(t, os.Symlink("initrd-6.6.20", filepath.Join(src, "initrd")))

	require.NoError(t, fillVolume(testLogger(), src, mountpoint))

	for _, tc := range []struct {
		name, link, target string
	}{
		{"relative alias", "vmlinuz", "kernel"},
		{"absolute target outside the rootfs", "escape", "/etc/shadow"},
		{"dangling target", "initrd", "initrd-6.6.20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(mountpoint, tc.link)
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
	mountpoint := t.TempDir()
	src := filepath.Join(extensionRootfs(t), "boot")
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644))

	require.NoError(t, fillVolume(testLogger(), src, mountpoint))

	_, err := os.Lstat(filepath.Join(mountpoint, "pipe"))
	assert.True(t, errors.Is(err, os.ErrNotExist), "a fifo must not be recreated")
	assert.FileExists(t, filepath.Join(mountpoint, "kernel"), "the rest of the tree still copies")
}

// TestFabricateBootVolume_NoKernelABIID pins the admission rule: a
// userspace-only extension declares no ABI, so it gets no volume and the
// engine is never called.
func TestFabricateBootVolume_NoKernelABIID(t *testing.T) {
	stub := newStubEngine(t)
	spec := specWith(map[string]string{
		"io.balena.image.class":  "overlay",
		"io.balena.service-name": "userspace-only",
	})

	source, err := fabricateBootVolume(context.Background(), testLogger(), spec, stored("sha256:42befc76f4f8aaaa"), extensionRootfs(t), "0123456789abcdef")
	require.NoError(t, err)

	assert.Empty(t, source)
	assert.Empty(t, stub.order, "an extension without a kernel must not reach the engine")
}

// TestFabricateBootVolume_ServiceNameFallback covers a manual deploy, where no
// service label exists and the container id has to name the volume.
func TestFabricateBootVolume_ServiceNameFallback(t *testing.T) {
	stub := newStubEngine(t)
	annotations := kernelOverride()
	delete(annotations, "io.balena.service-name")

	_, err := fabricateBootVolume(context.Background(), testLogger(), specWith(annotations), stored("sha256:42befc76f4f8aaaa"), extensionRootfs(t), "0123456789abcdeffedcba")
	require.NoError(t, err)

	assert.Equal(t, []string{"ext_0123456789ab_42befc76f4f8_boot"}, stub.order)
}

// TestFabricateBootVolume_StoreLabelsWinOverAnnotations closes the drift the
// two sides could otherwise develop. Cleanup's retention guard derives the
// volume's name from the engine's label map, so create has to derive it from
// the same map: a bundle whose annotations disagree must not move the name, or
// the guard holds back a name nothing carries and the sweep collects the /boot
// volume of a live extension.
//
// The two maps are equal in production only because the engine sets no
// annotations at all, which is a property of today's engine rather than of
// this contract.
func TestFabricateBootVolume_StoreLabelsWinOverAnnotations(t *testing.T) {
	stub := newStubEngine(t)

	// The bundle names a different service, and claims no kernel.
	spec := specWith(map[string]string{
		"io.balena.image.class":  "overlay",
		"io.balena.service-name": "from-the-bundle",
	})
	fromTheEngine := oci.StoredConfig{
		ImageID: "sha256:42befc76f4f8aaaa",
		Labels: map[string]string{
			"io.balena.image.class":         "overlay",
			"io.balena.image.kernel-abi-id": "6.6.20-integration",
			"io.balena.service-name":        "kernel-modules",
		},
	}

	source, err := fabricateBootVolume(context.Background(), testLogger(), spec,
		fromTheEngine, extensionRootfs(t), "0123456789abcdef")
	require.NoError(t, err)

	// The engine's labels admitted it and named it, exactly as the manager's
	// volume sweep re-derives from the same map.
	name := labels.VolumeName("kernel-modules", fromTheEngine.ImageID)
	assert.Equal(t, []string{name}, stub.order)
	assert.Equal(t, stub.mountpoint(name), source)
}

// TestFabricateBootVolume_Idempotent asserts a second create reuses the volume
// and leaves its contents alone: the create hook publishes the kernel into that
// volume, and a re-copy would overwrite what a rollback needs.
func TestFabricateBootVolume_Idempotent(t *testing.T) {
	stub := newStubEngine(t)
	rootfs := extensionRootfs(t)
	spec := specWith(kernelOverride())

	_, err := fabricateBootVolume(context.Background(), testLogger(), spec, stored("sha256:42befc76f4f8aaaa"), rootfs, "abc")
	require.NoError(t, err)

	name := "ext_kernel-modules_42befc76f4f8_boot"
	published := filepath.Join(stub.mountpoint(name), "published-by-hook")
	require.NoError(t, os.WriteFile(published, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stub.mountpoint(name), "kernel"), []byte("patched"), 0o644))

	_, err = fabricateBootVolume(context.Background(), testLogger(), spec, stored("sha256:42befc76f4f8aaaa"), rootfs, "abc")
	require.NoError(t, err)

	assert.Equal(t, []string{name}, stub.order, "the same image must key the same volume")
	assert.FileExists(t, published, "a filled volume must be left alone")
	kernel, err := os.ReadFile(filepath.Join(stub.mountpoint(name), "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "patched", string(kernel), "content must not be re-copied over")
}

// TestFabricateBootVolume_MarkerRecovery covers a kill mid-copy: the volume is
// neither empty nor complete, so emptiness alone would skip the refill
// forever. The marker is what makes the partial state recognisable.
func TestFabricateBootVolume_MarkerRecovery(t *testing.T) {
	stub := newStubEngine(t)
	rootfs := extensionRootfs(t)
	spec := specWith(kernelOverride())

	name := "ext_kernel-modules_42befc76f4f8_boot"
	mountpoint := stub.mountpoint(name)
	require.NoError(t, os.MkdirAll(mountpoint, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mountpoint, fabricatingMarker), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mountpoint, "kernel"), []byte("trunc"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mountpoint, "leftover"), []byte("junk"), 0o644))

	_, err := fabricateBootVolume(context.Background(), testLogger(), spec, stored("sha256:42befc76f4f8aaaa"), rootfs, "abc")
	require.NoError(t, err)

	kernel, err := os.ReadFile(filepath.Join(mountpoint, "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "vmlinuz", string(kernel), "a partial fill must be refilled from the rootfs")
	assert.NoFileExists(t, filepath.Join(mountpoint, "leftover"),
		"the wipe must clear what the interrupted copy left behind")
	assert.NoFileExists(t, filepath.Join(mountpoint, fabricatingMarker))
}

// TestFabricateBootVolume_MissingRootfsPath asserts a kernel override whose
// rootfs carries no /boot leaves an empty volume rather than failing the
// create, which is what the engine's own copy-on-create does.
func TestFabricateBootVolume_MissingRootfsPath(t *testing.T) {
	stub := newStubEngine(t)

	source, err := fabricateBootVolume(context.Background(), testLogger(), specWith(kernelOverride()), stored("sha256:42befc76f4f8aaaa"), t.TempDir(), "abc")
	require.NoError(t, err)

	require.NotEmpty(t, source)
	empty, err := isEmpty(stub.mountpoint("ext_kernel-modules_42befc76f4f8_boot"))
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

	_, err := fabricateBootVolume(context.Background(), testLogger(), specWith(kernelOverride()), stored("sha256:42befc76f4f8aaaa"), rootfs, "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), fabricatingMarker)
}

// TestFillVolume_ExcludesAConcurrentFill covers two containers deployed from
// the same service and image: they share one volume, and the engine serialises
// creates per container rather than per volume. Without exclusion one fill
// wipes the tree the other is copying into and both then finish believing the
// volume is complete.
func TestFillVolume_ExcludesAConcurrentFill(t *testing.T) {
	mountpoint := t.TempDir()
	src := filepath.Join(extensionRootfs(t), "boot")

	// Stand in for the other create, which holds the volume for its own fill.
	holder, err := os.Open(mountpoint)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX))

	done := make(chan error, 1)
	go func() { done <- fillVolume(testLogger(), src, mountpoint) }()

	select {
	case <-done:
		t.Fatal("a fill proceeded while another process held the volume")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_UN))
	require.NoError(t, holder.Close())
	require.NoError(t, <-done, "the fill must proceed once the volume is released")

	kernel, err := os.ReadFile(filepath.Join(mountpoint, "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "vmlinuz", string(kernel))
}

func TestFabricateBootVolume_EngineFailure(t *testing.T) {
	stub := newStubEngine(t)
	stub.err = errors.New("engine unavailable")

	_, err := fabricateBootVolume(context.Background(), testLogger(), specWith(kernelOverride()), stored("sha256:42befc76f4f8aaaa"), extensionRootfs(t), "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "engine unavailable")
}

// TestFabricateBootVolume_NoImageID asserts fabrication refuses to name a
// volume it cannot key on the image, rather than producing a name that
// collides across builds.
func TestFabricateBootVolume_NoImageID(t *testing.T) {
	newStubEngine(t)

	_, err := fabricateBootVolume(context.Background(), testLogger(), specWith(kernelOverride()), stored(""), extensionRootfs(t), "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image id")
}

// TestWithBootVolume_ReplacesSameDestination pins the ownership rule: the
// runtime owns /boot for a kernel override, so a mount the bundle happens to
// declare there is replaced rather than joined, and the hook sees one
// EXTENSION_VOLUME_BOOT rather than two to choose between.
func TestWithBootVolume_ReplacesSameDestination(t *testing.T) {
	spec := []specs.Mount{
		{Destination: "/etc/hosts", Source: "/host/hosts"},
		{Destination: "/boot", Source: "/stale/boot"},
	}

	merged := withBootVolume(spec, "/volumes/ext_svc_abc_boot/_data")
	require.Len(t, merged, 2)
	assert.Equal(t, "/etc/hosts", merged[0].Destination)
	assert.Equal(t, "/boot", merged[1].Destination)
	assert.Equal(t, "/volumes/ext_svc_abc_boot/_data", merged[1].Source)
	assert.Equal(t, "/stale/boot", spec[1].Source, "the caller's slice must not be modified in place")

	assert.Equal(t, spec, withBootVolume(spec, ""))
}
