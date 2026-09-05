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

func TestEnrichAnnotations_LabelsAndImageID(t *testing.T) {
	writeContainerConfig(t, "abc123", `{
		"Image": "sha256:42befc76f4f8e9a1c0d3b5a7e2f4c6d8a0b2c4e6f8a0b2c4e6f8a0b2c4e6f8a0",
		"Config": {"Labels": {
			"io.balena.image.class": "overlay",
			"io.balena.image.kernel-abi-id": "6.6.20-abi"
		}}
	}`)

	spec := &specs.Spec{}
	stored := EnrichAnnotations(testLogger(), spec, "abc123")

	assert.Equal(t, "sha256:42befc76f4f8e9a1c0d3b5a7e2f4c6d8a0b2c4e6f8a0b2c4e6f8a0b2c4e6f8a0", stored.ImageID)
	assert.Equal(t, "overlay", spec.Annotations["io.balena.image.class"])
	assert.Equal(t, "6.6.20-abi", spec.Annotations["io.balena.image.kernel-abi-id"])

	// The engine's own label map is returned alongside, because volume
	// identity is derived from it rather than from the annotations.
	assert.Equal(t, map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-abi",
	}, stored.Labels)
}

// TestEnrichAnnotations_KeepsSpecAnnotations asserts annotations already on
// the spec win for the hook environment, while the store's own view is still
// surfaced unchanged: volume identity is derived from that view, so a bundle
// overriding the annotations must not move it.
func TestEnrichAnnotations_KeepsSpecAnnotations(t *testing.T) {
	writeContainerConfig(t, "abc123", `{
		"Image": "sha256:0123456789abcdef",
		"Config": {"Labels": {"io.balena.image.class": "from-store"}}
	}`)

	spec := &specs.Spec{Annotations: map[string]string{"io.balena.image.class": "from-spec"}}
	stored := EnrichAnnotations(testLogger(), spec, "abc123")

	assert.Equal(t, "sha256:0123456789abcdef", stored.ImageID)
	assert.Equal(t, "from-spec", spec.Annotations["io.balena.image.class"])
	assert.Equal(t, "from-store", stored.Labels["io.balena.image.class"])
}

func TestEnrichAnnotations_MissingConfig(t *testing.T) {
	writeContainerConfig(t, "other", `{"Image":"sha256:dead","Config":{"Labels":{}}}`)

	spec := &specs.Spec{}
	assert.Empty(t, EnrichAnnotations(testLogger(), spec, "abc123").ImageID)
	assert.Empty(t, spec.Annotations)
}

// TestEnrichAnnotations_InvalidContainerID asserts a crafted id never reaches
// a path join.
func TestEnrichAnnotations_InvalidContainerID(t *testing.T) {
	spec := &specs.Spec{}
	assert.Empty(t, EnrichAnnotations(testLogger(), spec, "../../etc").ImageID)
	assert.Empty(t, spec.Annotations)
}
