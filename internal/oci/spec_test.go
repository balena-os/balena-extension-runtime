package oci

import (
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
