package mounts

import (
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
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
