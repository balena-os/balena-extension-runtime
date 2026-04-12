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
		Class:                          ClassOverlay,
		KernelABIID:                    "sha256:abc123",
		KernelVersion:                  "6.12.61",
		OSVersion:                      "2.119.*",
		Prefix + "future-thing":        "x",
		"unrelated":                    "ignored",
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
