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
