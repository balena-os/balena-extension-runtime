package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunningSlot(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the mount sources and the by-label links.
		setup func(t *testing.T, w *world)
		want  bootenv.Slot
	}{
		{
			name:  "a resin-prefixed label",
			setup: func(t *testing.T, w *world) { w.slotIs("resin-rootA") },
			want:  bootenv.SlotA,
		},
		{
			name:  "a balena-prefixed label",
			setup: func(t *testing.T, w *world) { w.slotIs("balena-rootB") },
			want:  bootenv.SlotB,
		},
		{
			// Bind-mounted over itself while old hooks run from the inactive
			// partition. Both entries name the same source.
			name: "a duplicated mount entry",
			setup: func(t *testing.T, w *world) {
				w.slotIs("resin-rootB")
				device := filepath.Join(w.root, "dev", "sda2")
				w.mountedAs(device, device)
			},
			want: bootenv.SlotB,
		},
		{
			// On LUKS the mounted source is the dm device and by-label
			// publishes the inner filesystem's label.
			name: "a LUKS dm source",
			setup: func(t *testing.T, w *world) {
				dm := filepath.Join(w.root, "dev", "dm-0")
				require.NoError(t, os.MkdirAll(filepath.Dir(dm), 0o755))
				require.NoError(t, os.WriteFile(dm, nil, 0o644))
				mapper := filepath.Join(w.root, "dev", "mapper")
				require.NoError(t, os.MkdirAll(mapper, 0o755))
				require.NoError(t, os.Symlink(dm, filepath.Join(mapper, "resin-rootA")))

				w.mountedAs(filepath.Join(mapper, "resin-rootA"))
				w.labels(map[string]string{"resin-rootA": dm})
			},
			want: bootenv.SlotA,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(t, w)

			slot, err := RunningSlot()
			require.NoError(t, err)
			assert.Equal(t, tc.want, slot)
		})
	}
}

// Writing the wrong slot's committed value is how a proven kernel gets
// retired, so an unnameable slot refuses rather than guesses.
func TestRunningSlot_RefusesAnUnnameableSource(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, w *world)
	}{
		{
			name: "no candidate label resolves to the source",
			setup: func(t *testing.T, w *world) {
				w.slotIs("resin-rootA")
				w.labels(map[string]string{"resin-rootA": filepath.Join(w.root, "dev", "sdb1")})
			},
		},
		{
			name: "the running root is not in the mount table",
			setup: func(t *testing.T, w *world) {
				require.NoError(t, os.WriteFile(procMounts, []byte("proc /proc proc rw 0 0\n"), 0o644))
			},
		},
		{
			name: "no by-label directory at all",
			setup: func(t *testing.T, w *world) {
				require.NoError(t, os.RemoveAll(byLabelDir))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(t, w)

			_, err := RunningSlot()
			assert.Error(t, err)
		})
	}
}

// The relay is consumed and the records are swept before the slot is needed,
// so a device that cannot name its slot still gets both.
func TestRun_SweepsBeforeRefusingAnUnnameableSlot(t *testing.T) {
	w := newWorld(t, bootenv.KeyABI+"="+abiX)
	w.publish(abiX)
	w.labels(nil)

	assert.Error(t, w.run())
	assert.Empty(t, w.block())
	assert.Empty(t, w.published())
}
