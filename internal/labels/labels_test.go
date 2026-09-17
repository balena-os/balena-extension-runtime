package labels

import (
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

// TestBootVolume covers the name both create and the retention guard derive,
// including the service fallback and the missing image id.
func TestBootVolume(t *testing.T) {
	tests := []struct {
		name        string
		lbls        map[string]string
		containerID string
		imageID     string
		want        string
		wantErr     string
	}{
		{
			name:        "not owed",
			lbls:        map[string]string{Class: ClassOverlay},
			containerID: "0123456789abcdeffedcba",
			imageID:     "sha256:42befc76f4f8aaaa",
		},
		{
			name: "service label",
			lbls: map[string]string{
				KernelABIID: "6.6.20-abc",
				ServiceName: "kernel-modules",
			},
			containerID: "0123456789abcdeffedcba",
			imageID:     "sha256:42befc76f4f8aaaa",
			want:        "ext_kernel-modules_42befc76f4f8_boot",
		},
		{
			name:        "container id fallback",
			lbls:        map[string]string{KernelABIID: "6.6.20-abc"},
			containerID: "0123456789abcdeffedcba",
			imageID:     "sha256:42befc76f4f8aaaa",
			want:        "ext_0123456789ab_42befc76f4f8_boot",
		},
		{
			name:        "no image id",
			lbls:        map[string]string{KernelABIID: "6.6.20-abc"},
			containerID: "0123456789abcdeffedcba",
			wantErr:     "image id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, err := BootVolume(tt.lbls, tt.containerID, tt.imageID)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, name)
		})
	}
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
