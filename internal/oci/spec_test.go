package oci

import (
	"os"
	"path/filepath"
	"testing"

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

	rootfs := ResolveRootfs(spec, bundle)
	assert.Equal(t, filepath.Join(bundle, "rootfs"), rootfs)
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

	rootfs := ResolveRootfs(spec, bundle)
	assert.Equal(t, "/var/lib/docker/overlay2/abc/merged", rootfs)
}
