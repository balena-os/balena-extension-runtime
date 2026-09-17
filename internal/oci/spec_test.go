package oci

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadSpec(t *testing.T) {
	bundle := t.TempDir()
	configJSON := `{
		"ociVersion": "1.0.2",
		"root": { "path": "rootfs", "readonly": true },
		"process": { "args": ["none"] },
		"annotations": {
			"io.balena.image.class": "overlay"
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), []byte(configJSON), 0o644))

	spec, err := ReadSpec(bundle)
	require.NoError(t, err)
	assert.Equal(t, "rootfs", spec.Root.Path)
	assert.Equal(t, "overlay", spec.Annotations["io.balena.image.class"])
}

func TestReadSpecMissing(t *testing.T) {
	_, err := ReadSpec(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config.json")
}

func TestResolveRootfsRelative(t *testing.T) {
	bundle := "/var/run/containerd/io.containerd.runtime.v2.task/moby/abc123"
	configJSON := `{
		"ociVersion": "1.0.2",
		"root": { "path": "rootfs" },
		"process": { "args": ["none"] }
	}`
	tmpBundle := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpBundle, "config.json"), []byte(configJSON), 0o644))

	spec, err := ReadSpec(tmpBundle)
	require.NoError(t, err)

	rootfs, err := ResolveRootfs(spec, bundle)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(bundle, "rootfs"), rootfs)
}

func TestResolveRootfsTraversalRejected(t *testing.T) {
	bundle := t.TempDir()
	configJSON := `{
		"ociVersion": "1.0.2",
		"root": { "path": "../../etc" },
		"process": { "args": ["none"] }
	}`
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), []byte(configJSON), 0o644))

	spec, err := ReadSpec(bundle)
	require.NoError(t, err)

	_, err = ResolveRootfs(spec, bundle)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes bundle")
}

func TestResolveRootfsEmptyRejected(t *testing.T) {
	spec := &specs.Spec{Root: &specs.Root{Path: ""}}
	_, err := ResolveRootfs(spec, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")

	_, err = ResolveRootfs(&specs.Spec{}, t.TempDir())
	require.Error(t, err, "nil Root must be rejected")
}

func TestNormalizeBundlePath(t *testing.T) {
	got, err := NormalizeBundlePath("/var/lib/docker/./overlay/../extensions")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/docker/extensions", got)

	_, err = NormalizeBundlePath("")
	require.Error(t, err)
}

func TestResolveRootfsAbsolute(t *testing.T) {
	bundle := t.TempDir()
	configJSON := `{
		"ociVersion": "1.0.2",
		"root": { "path": "/var/lib/docker/overlay2/abc/merged" },
		"process": { "args": ["none"] }
	}`
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), []byte(configJSON), 0o644))

	spec, err := ReadSpec(bundle)
	require.NoError(t, err)

	rootfs, err := ResolveRootfs(spec, bundle)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/docker/overlay2/abc/merged", rootfs)
}

// writeContainerConfig writes a config.v2.json fixture into a fresh docker
// root and points the package at it for the duration of the test.
func writeContainerConfig(t *testing.T, containerID, body string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "containers", containerID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.v2.json"), []byte(body), 0o644))

	prev := getDockerRoot()
	SetDockerRoot(root)
	t.Cleanup(func() { SetDockerRoot(prev) })
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReadIdentity_StoreWins(t *testing.T) {
	writeContainerConfig(t, "abc123", `{
		"Image": "sha256:0123456789abcdef",
		"Config": {"Labels": {
			"io.balena.image.class": "from-store",
			"io.balena.image.kernel-abi-id": "6.6.20-abi"
		}}
	}`)

	spec := &specs.Spec{Annotations: map[string]string{"io.balena.image.class": "from-spec"}}
	id := ReadIdentity(testLogger(), spec, "abc123")

	assert.Equal(t, map[string]string{
		"io.balena.image.class":         "from-store",
		"io.balena.image.kernel-abi-id": "6.6.20-abi",
	}, id.Labels)
	assert.Equal(t, "sha256:0123456789abcdef", id.ImageID)
	assert.Equal(t, map[string]string{"io.balena.image.class": "from-spec"}, spec.Annotations)
}

func TestReadIdentity_NoStoreFallsBackToTheSpec(t *testing.T) {
	writeContainerConfig(t, "other", `{"Image":"sha256:dead","Config":{"Labels":{"io.balena.image.class":"from-store"}}}`)

	spec := &specs.Spec{Annotations: map[string]string{"io.balena.image.class": "from-spec"}}
	id := ReadIdentity(testLogger(), spec, "abc123")

	assert.Equal(t, spec.Annotations, id.Labels)
	assert.Empty(t, id.ImageID)
}

// TestReadIdentity_StoreWithNoLabels asserts both fields come from one source.
// A store container with no labels gets no spec fallback.
func TestReadIdentity_StoreWithNoLabels(t *testing.T) {
	writeContainerConfig(t, "abc123", `{"Image":"sha256:0123456789abcdef","Config":{"Labels":{}}}`)

	spec := &specs.Spec{Annotations: map[string]string{"io.balena.image.class": "from-spec"}}
	id := ReadIdentity(testLogger(), spec, "abc123")

	assert.Empty(t, id.Labels)
	assert.Equal(t, "sha256:0123456789abcdef", id.ImageID)
}

// TestReadIdentity_InvalidContainerID asserts a crafted id never reaches a
// path join.
func TestReadIdentity_InvalidContainerID(t *testing.T) {
	spec := &specs.Spec{Annotations: map[string]string{"io.balena.image.class": "from-spec"}}
	id := ReadIdentity(testLogger(), spec, "../../etc")

	assert.Equal(t, spec.Annotations, id.Labels)
	assert.Empty(t, id.ImageID)
}

func TestVolumeDataDir(t *testing.T) {
	root := t.TempDir()
	prev := getDockerRoot()
	SetDockerRoot(root)
	t.Cleanup(func() { SetDockerRoot(prev) })

	got, err := VolumeDataDir("ext_svc_abc_boot")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "volumes", "ext_svc_abc_boot", "_data"), got)
}

func TestVolumeRelDir_RefusesNonBareNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "/abs"} {
		_, err := VolumeRelDir(name)
		require.Error(t, err, "name %q must be refused", name)
		assert.Contains(t, err.Error(), "bare file name")
	}
}
