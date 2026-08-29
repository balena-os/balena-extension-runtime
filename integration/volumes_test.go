package integration_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildExtensionImageWithContent imports an image whose rootfs holds the given
// files, so the runtime has something real to copy into a fabricated volume.
// Paths are relative to the rootfs, for example "boot/kernel".
func buildExtensionImageWithContent(t *testing.T, tag string, files map[string]string, extraLabels ...string) {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for path, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     path,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())

	args := []string{"import", "--change", "LABEL io.balena.image.class=overlay"}
	for _, l := range extraLabels {
		args = append(args, "--change", "LABEL "+l)
	}
	args = append(args, "-", tag)

	cmd := exec.Command("docker", args...)
	cmd.Stdin = bytes.NewReader(buf.Bytes())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("buildExtensionImageWithContent(%s): %v\n%s", tag, err, out)
	}
}

// runExtension creates and runs a container through the extension runtime,
// returning its id. The runtime's create is what fabricates the volume, and
// the engine only invokes it on start, so the container has to be run rather
// than merely created.
func runExtension(t *testing.T, tag, service string) string {
	t.Helper()
	id := dockerExec(t, "create",
		"--runtime", "extension",
		"--label", "io.balena.service-name="+service,
		tag, "none")
	t.Cleanup(func() { dockerExecMayFail(t, "rm", "-f", id) })

	out, err := dockerExecMayFail(t, "start", id)
	require.NoError(t, err, "start through the extension runtime failed: %s", out)

	code := dockerExec(t, "wait", id)
	require.Equal(t, "0", code, "extension container must exit zero")
	return id
}

// imageID returns the container's image id, which the fabricated volume is
// named after and which cleanup's retention guard re-derives that name from.
func imageID(t *testing.T, containerID string) string {
	t.Helper()
	return dockerExec(t, "inspect", "--format", "{{.Image}}", containerID)
}

// imageDigest12 returns the first 12 characters of the container's image id,
// which is what the volume name is keyed on.
func imageDigest12(t *testing.T, containerID string) string {
	t.Helper()
	return strings.TrimPrefix(imageID(t, containerID), "sha256:")[:12]
}

func volumeMountpoint(t *testing.T, name string) string {
	t.Helper()
	return dockerExec(t, "volume", "inspect", "--format", "{{.Mountpoint}}", name)
}

func volumeLabels(t *testing.T, name string) map[string]string {
	t.Helper()
	out := dockerExec(t, "volume", "inspect", "--format", "{{json .Labels}}", name)
	var got map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	return got
}

// TestFabricate_KernelOverride is the whole mechanism end to end against a
// real engine: the kernel ABI id admits the extension, the runtime creates the
// /boot volume, fills it from the image, and leaves it attached to nothing.
func TestFabricate_KernelOverride(t *testing.T) {
	tag := uniqueName("ext-kernel")
	buildExtensionImageWithContent(t, tag,
		map[string]string{"boot/kernel": "vmlinuz", "boot/dtb/board.dtb": "fdt"},
		"io.balena.image.kernel-abi-id=6.6.20-integration",
		"io.balena.image.kernel-version=6.6.20")
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	service := uniqueName("kernel-modules")
	id := runExtension(t, tag, service)

	name := "ext_" + service + "_" + imageDigest12(t, id) + "_boot"
	defer dockerExecMayFail(t, "volume", "rm", "-f", name)

	mountpoint := volumeMountpoint(t, name)
	content, err := os.ReadFile(filepath.Join(mountpoint, "kernel"))
	require.NoError(t, err, "volume must be filled from the image rootfs")
	assert.Equal(t, "vmlinuz", string(content))

	nested, err := os.ReadFile(filepath.Join(mountpoint, "dtb", "board.dtb"))
	require.NoError(t, err)
	assert.Equal(t, "fdt", string(nested))

	// The image labels are what the commit sweep applies its staleness
	// predicate to, so a volume without them would never be collected. They
	// are all the volume carries: everything that has to reach the volume
	// re-derives its name, so no bookkeeping label has to survive on it.
	got := volumeLabels(t, name)
	assert.Equal(t, "overlay", got["io.balena.image.class"])
	assert.Equal(t, "6.6.20-integration", got["io.balena.image.kernel-abi-id"])
	assert.Equal(t, "6.6.20", got["io.balena.image.kernel-version"])
	assert.NotContains(t, got, "io.balena.service-name",
		"only image labels are copied; the deployment label is not one")

	mounts := dockerExec(t, "inspect", "--format", "{{json .Mounts}}", id)
	assert.Equal(t, "[]", mounts,
		"the fabricated volume is dangling from birth and never attached to the container")
}

// TestFabricate_NoKernelABIID pins the admission rule against a real engine: a
// userspace-only extension declares no ABI, so no volume is fabricated and no
// anonymous one appears in its place.
func TestFabricate_NoKernelABIID(t *testing.T) {
	tag := uniqueName("ext-userspace")
	buildExtensionImageWithContent(t, tag, map[string]string{"boot/kernel": "vmlinuz"})
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	service := uniqueName("userspace-only")
	id := runExtension(t, tag, service)

	for _, name := range strings.Split(dockerExec(t, "volume", "ls", "--format", "{{.Name}}"), "\n") {
		assert.NotContains(t, name, service,
			"no volume may be fabricated for an extension that carries no kernel")
	}
	assert.Equal(t, "[]", dockerExec(t, "inspect", "--format", "{{json .Mounts}}", id))
}

// TestFabricate_Idempotent asserts a redeploy of the same image reuses the
// volume and leaves its contents alone. The create hook publishes the kernel
// into that volume, so a re-copy would overwrite what a rollback needs.
func TestFabricate_Idempotent(t *testing.T) {
	tag := uniqueName("ext-again")
	buildExtensionImageWithContent(t, tag,
		map[string]string{"boot/kernel": "vmlinuz"},
		"io.balena.image.kernel-abi-id=6.6.20-integration")
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	service := uniqueName("idempotent")
	first := runExtension(t, tag, service)

	name := "ext_" + service + "_" + imageDigest12(t, first) + "_boot"
	defer dockerExecMayFail(t, "volume", "rm", "-f", name)

	mountpoint := volumeMountpoint(t, name)
	published := filepath.Join(mountpoint, "published-by-hook")
	require.NoError(t, os.WriteFile(published, []byte("state"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(mountpoint, "kernel"), []byte("patched"), 0o644))

	second := runExtension(t, tag, service)
	assert.Equal(t, name, "ext_"+service+"_"+imageDigest12(t, second)+"_boot",
		"the same image must key the same volume name")

	assert.FileExists(t, published, "a filled volume must survive a second create")
	kernel, err := os.ReadFile(filepath.Join(mountpoint, "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "patched", string(kernel), "content must not be re-copied over")

	listed := dockerExec(t, "volume", "ls", "--filter", "name="+name, "--format", "{{.Name}}")
	assert.Equal(t, name, listed, "the second create must not add a second volume")
}

// TestFabricate_FoundByDerivedName walks the derivation against a real engine.
// The container reports no mounts, so re-deriving the name from its labels and
// image id is the only route back to the volume: a redeploy takes it to reuse
// the volume, and cleanup's retention guard to recognise the volume as still
// spoken for. That name has to address exactly one volume on a device carrying
// more than one.
func TestFabricate_FoundByDerivedName(t *testing.T) {
	tag := uniqueName("ext-identity")
	buildExtensionImageWithContent(t, tag,
		map[string]string{"boot/kernel": "vmlinuz"},
		"io.balena.image.kernel-abi-id=6.6.20-identity")
	defer dockerExecMayFail(t, "rmi", "-f", tag)

	service := uniqueName("kernel-modules")
	id := runExtension(t, tag, service)
	name := "ext_" + service + "_" + imageDigest12(t, id) + "_boot"
	defer dockerExecMayFail(t, "volume", "rm", "-f", name)

	// A second extension, so a lookup that matched too broadly would show up
	// here rather than passing on a single-volume device.
	otherTag := uniqueName("ext-other")
	buildExtensionImageWithContent(t, otherTag,
		map[string]string{"boot/kernel": "other"},
		"io.balena.image.kernel-abi-id=6.6.20-other")
	defer dockerExecMayFail(t, "rmi", "-f", otherTag)
	otherService := uniqueName("other-modules")
	otherID := runExtension(t, otherTag, otherService)
	otherName := "ext_" + otherService + "_" + imageDigest12(t, otherID) + "_boot"
	defer dockerExecMayFail(t, "volume", "rm", "-f", otherName)

	require.NotEqual(t, name, otherName, "two deployments must not share a volume")

	// The derived name is what the daemon resolves, and it reaches this
	// extension's kernel rather than the other's.
	content, err := os.ReadFile(filepath.Join(volumeMountpoint(t, name), "kernel"))
	require.NoError(t, err)
	assert.Equal(t, "vmlinuz", string(content))
}
