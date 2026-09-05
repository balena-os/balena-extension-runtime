package mounts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToEnv_Empty(t *testing.T) {
	assert.Empty(t, ToEnv(nil))
	assert.Empty(t, ToEnv([]specs.Mount{}))
}

func TestToEnv_SingleBootVolume(t *testing.T) {
	mounts := []specs.Mount{
		{Destination: "/boot", Source: "/mnt/data/docker/volumes/abc/_data", Type: "bind"},
	}
	got := ToEnv(mounts)
	assert.Equal(t, []string{"EXTENSION_VOLUME_BOOT=/mnt/data/docker/volumes/abc/_data"}, got)
}

func TestToEnv_MultipleSortedDeterministic(t *testing.T) {
	mounts := []specs.Mount{
		{Destination: "/zzz", Source: "/z"},
		{Destination: "/boot", Source: "/b"},
		{Destination: "/var/lib/foo", Source: "/v"},
	}
	got := ToEnv(mounts)
	assert.Equal(t, []string{
		"EXTENSION_VOLUME_BOOT=/b",
		"EXTENSION_VOLUME_VAR_LIB_FOO=/v",
		"EXTENSION_VOLUME_ZZZ=/z",
	}, got)
}

func TestToEnv_SkipsNonAbsoluteDestinations(t *testing.T) {
	mounts := []specs.Mount{
		{Destination: "boot", Source: "/x"},
		{Destination: "", Source: "/y"},
		{Destination: "/ok", Source: "/o"},
	}
	got := ToEnv(mounts)
	assert.Equal(t, []string{"EXTENSION_VOLUME_OK=/o"}, got)
}

func TestToEnv_NormalizesPathSeparators(t *testing.T) {
	mounts := []specs.Mount{
		{Destination: "/a/b-c/d", Source: "/x"},
	}
	got := ToEnv(mounts)
	assert.Equal(t, []string{"EXTENSION_VOLUME_A_B_C_D=/x"}, got)
}

func TestToEnv_SkipsRootDestination(t *testing.T) {
	mounts := []specs.Mount{
		{Destination: "/", Source: "/dev/sda1"},
		{Destination: "/ok", Source: "/o"},
	}
	got := ToEnv(mounts)
	assert.Equal(t, []string{"EXTENSION_VOLUME_OK=/o"}, got)
}

func TestIsMounted(t *testing.T) {
	table := filepath.Join(t.TempDir(), "mounts")
	require.NoError(t, os.WriteFile(table, []byte(
		"/dev/mmcblk0p1 /mnt/boot vfat rw,relatime 0 0\n"+
			"/dev/mmcblk0p5 /mnt/state ext4 rw,relatime 0 0\n"), 0o644))

	prev := procMounts
	procMounts = table
	t.Cleanup(func() { procMounts = prev })

	mounted, err := IsMounted("/mnt/state")
	require.NoError(t, err)
	assert.True(t, mounted)

	mounted, err = IsMounted("/mnt/data")
	require.NoError(t, err)
	assert.False(t, mounted, "a path that is not a mountpoint is not mounted")

	// A mountpoint that is a prefix of another must not match it.
	mounted, err = IsMounted("/mnt")
	require.NoError(t, err)
	assert.False(t, mounted)
}

// An unreadable mount table is a machine condition the caller has to be able
// to tell apart from "not mounted".
func TestIsMounted_UnreadableTableErrors(t *testing.T) {
	prev := procMounts
	procMounts = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { procMounts = prev })

	_, err := IsMounted("/mnt/state")
	assert.Error(t, err)
}
