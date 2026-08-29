package labels

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantErr     string
	}{
		{
			name:        "valid overlay",
			annotations: map[string]string{Class: ClassOverlay},
		},
		{
			name:        "with extra labels",
			annotations: map[string]string{Class: ClassOverlay, KernelABIID: "sha256:abc123"},
		},
		{
			name:        "missing class",
			annotations: map[string]string{"other": "value"},
			wantErr:     "missing required label",
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			wantErr:     "missing required label",
		},
		{
			name:        "nil annotations",
			annotations: nil,
			wantErr:     "missing required label",
		},
		{
			name:        "wrong class value",
			annotations: map[string]string{Class: "volume"},
			wantErr:     "unsupported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.annotations)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestToEnv(t *testing.T) {
	// ToEnv must forward every io.balena.image.* annotation as
	// EXTENSION_IMAGE_* regardless of whether the runtime has a named
	// constant for it. Include an arbitrary-name annotation to prove the
	// forwarding is prefix-based, not a fixed allowlist.
	annotations := map[string]string{
		Class:                   ClassOverlay,
		KernelABIID:             "sha256:abc123",
		KernelVersion:           "6.12.61",
		OSVersion:               "2.119.*",
		Prefix + "future-thing": "x",
		"unrelated":             "ignored",
	}

	env := ToEnv(annotations)
	sort.Strings(env)

	expected := []string{
		"EXTENSION_IMAGE_CLASS=overlay",
		"EXTENSION_IMAGE_FUTURE_THING=x",
		"EXTENSION_IMAGE_KERNEL_ABI_ID=sha256:abc123",
		"EXTENSION_IMAGE_KERNEL_VERSION=6.12.61",
		"EXTENSION_IMAGE_OS_VERSION=2.119.*",
	}
	sort.Strings(expected)

	assert.Equal(t, expected, env)
}

func TestToEnvEmpty(t *testing.T) {
	env := ToEnv(map[string]string{"other": "value"})
	assert.Empty(t, env)
}

// TestResolveServiceName_PrefersTheLabel covers the ordinary deploy, where the
// compose service name is what the volume is keyed on.
func TestResolveServiceName_PrefersTheLabel(t *testing.T) {
	name, fellBack := ResolveServiceName(
		map[string]string{ServiceName: "kernel-modules"}, "0123456789abcdef")
	assert.Equal(t, "kernel-modules", name)
	assert.False(t, fellBack)
}

// TestResolveServiceName_FallsBackToContainerID covers a manual deploy that
// carries no service name.
func TestResolveServiceName_FallsBackToContainerID(t *testing.T) {
	name, fellBack := ResolveServiceName(nil, "0123456789abcdef0000")
	assert.Equal(t, "0123456789ab", name)
	assert.True(t, fellBack)
}

// TestResolveServiceName_ShortContainerIDIsUsedWhole guards the slice: an id
// shorter than the fallback width must not panic.
func TestResolveServiceName_ShortContainerIDIsUsedWhole(t *testing.T) {
	name, fellBack := ResolveServiceName(map[string]string{ServiceName: ""}, "abc")
	assert.Equal(t, "abc", name)
	assert.True(t, fellBack)
}

// TestResolveServiceName_IsStableAcrossCallers is the property both sides
// depend on: fabrication and cleanup's retention guard derive the same key
// from the same container, or the volume cannot be found again.
func TestResolveServiceName_IsStableAcrossCallers(t *testing.T) {
	lbls := map[string]string{"io.balena.image.class": "overlay"}
	first, _ := ResolveServiceName(lbls, "deadbeefcafe0000")
	second, _ := ResolveServiceName(lbls, "deadbeefcafe0000")
	assert.Equal(t, first, second)
}

// TestVolumeName_Format is a worked example of the name, so the shape is
// readable without deriving it by hand.
func TestVolumeName_Format(t *testing.T) {
	name := VolumeName("kernel-modules",
		"sha256:42befc76f4f8e9a1c0d3b5a7e2f4c6d8a0b2c4e6f8a0b2c4e6f8a0b2c4e6f8a0")
	assert.Equal(t, "ext_kernel-modules_42befc76f4f8_boot", name)
}

// TestVolumeName_DistinctPerImage asserts a new build keys a new volume, which
// is what lets the previous one survive the validation window.
func TestVolumeName_DistinctPerImage(t *testing.T) {
	first := VolumeName("kernel-modules", "sha256:42befc76f4f8aaaaaaaa")
	second := VolumeName("kernel-modules", "sha256:0f1e2d3c4b5aaaaaaaaa")

	assert.Equal(t, "ext_kernel-modules_42befc76f4f8_boot", first)
	assert.Equal(t, "ext_kernel-modules_0f1e2d3c4b5a_boot", second)
	assert.NotEqual(t, first, second)
}

// TestVolumeName_ShortImageID asserts an id shorter than the digest width is
// used whole rather than panicking on the slice.
func TestVolumeName_ShortImageID(t *testing.T) {
	assert.Equal(t, "ext_svc_abc_boot", VolumeName("svc", "sha256:abc"))
}

// TestVolumeName_MatchesTheServiceFallback is the composed contract the
// retention guard relies on for a manual deploy.
func TestVolumeName_MatchesTheServiceFallback(t *testing.T) {
	service, fellBack := ResolveServiceName(nil, "0123456789abcdeffedcba")
	require.True(t, fellBack)
	assert.Equal(t, "ext_0123456789ab_42befc76f4f8_boot",
		VolumeName(service, "sha256:42befc76f4f8aaaa"))
}

// TestFabricatesVolume is the admission rule create and cleanup share. A
// drift between them either derives a name for a volume that was never made,
// or none for one that was.
func TestFabricatesVolume(t *testing.T) {
	assert.True(t, FabricatesVolume(map[string]string{KernelABIID: "6.6.20-abc"}))
	assert.False(t, FabricatesVolume(map[string]string{Class: ClassOverlay}))
	assert.False(t, FabricatesVolume(map[string]string{KernelABIID: ""}))
	assert.False(t, FabricatesVolume(nil))
}

func TestImage_FiltersToPrefix(t *testing.T) {
	selected := Image(map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-abc",
		"io.balena.service-name":        "kernel-modules",
		"io.balena.supervised":          "true",
		"maintainer":                    "someone",
	})

	assert.Equal(t, map[string]string{
		"io.balena.image.class":         "overlay",
		"io.balena.image.kernel-abi-id": "6.6.20-abc",
	}, selected)
}
