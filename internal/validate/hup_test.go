package validate

import (
	"context"
	"os"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (w *world) commit() error {
	w.t.Helper()
	return HUPCommit(context.Background(), w.logger)
}

func (w *world) undo() error {
	w.t.Helper()
	return HUPReject(context.Background(), w.logger)
}

// An empty arm and one that did not boot are the same case: no override took
// effect, so no committed key is written.
func TestHUPCommit(t *testing.T) {
	t.Run("an empty arm writes nothing", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyUpgradeAvailable+"=1")

		require.NoError(t, w.commit())
		assert.Equal(t, map[string]string{bootenv.KeyUpgradeAvailable: "1"}, w.block())
		assert.Zero(t, w.locks)
	})

	t.Run("a running arm is committed for the running slot", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX, bootenv.KeyTrial+"=2")
		w.runningIs(abiX)

		require.NoError(t, w.commit())
		assert.Equal(t, map[string]string{
			bootenv.KeyABI:                      abiX,
			bootenv.KeyCommitted(bootenv.SlotA): abiX,
		}, w.block())
	})

	t.Run("an arm that did not boot is not committed", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX)
		w.runningIs(abiY)

		require.NoError(t, w.commit())
		assert.Equal(t, map[string]string{bootenv.KeyABI: abiX}, w.block())
		assert.Zero(t, w.locks)
	})

	t.Run("the prestate survives for the next boot's validator", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX)
		w.runningIs(abiX)
		w.writePrestate()

		require.NoError(t, w.commit())
		assert.True(t, w.prestateExists(), "rollback-health leaves it; the next arm replaces it")
	})

	t.Run("no block exits zero", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX)
		require.NoError(t, os.Remove(w.blockPath))

		assert.NoError(t, w.commit())
	})
}

// The rollback lands in the slot the running one is not, and the relay is
// what the next boot reads to finish undoing the window.
func TestHUPReject(t *testing.T) {
	tests := []struct {
		name      string
		entries   []string
		running   string
		wantBlock map[string]string
		wantAudit string
	}{
		{
			// The arm booted; the slot proved another override.
			name: "an arm that booted is relayed and the target's override restored",
			entries: []string{
				bootenv.KeyABI + "=" + abiX,
				bootenv.KeyCommitted(bootenv.SlotB) + "=" + abiY,
				bootenv.KeyTrial + "=2",
			},
			running: abiX,
			wantBlock: map[string]string{
				bootenv.KeyABI:                      abiY,
				bootenv.KeyCommitted(bootenv.SlotB): abiY,
				bootenv.KeyRejected:                 abiX,
				bootenv.KeyTrial:                    "2",
			},
			wantAudit: "by=health from=" + abiX + " to=" + abiY + " slot=B",
		},
		{
			// Relaying would forget an ABI this slot has proven.
			name: "a redundant rollback relays nothing",
			entries: []string{
				bootenv.KeyABI + "=" + abiX,
				bootenv.KeyCommitted(bootenv.SlotB) + "=" + abiX,
			},
			running: abiX,
			wantBlock: map[string]string{
				bootenv.KeyABI:                      abiX,
				bootenv.KeyCommitted(bootenv.SlotB): abiX,
			},
			wantAudit: "by=health from=" + abiX + " to=" + abiX + " slot=B",
		},
		{
			// Nothing was proven about kernel bytes that never ran.
			name: "an arm that never took effect relays nothing",
			entries: []string{
				bootenv.KeyABI + "=" + abiX,
				bootenv.KeyCommitted(bootenv.SlotB) + "=" + abiY,
			},
			running: "",
			wantBlock: map[string]string{
				bootenv.KeyABI:                      abiY,
				bootenv.KeyCommitted(bootenv.SlotB): abiY,
			},
			wantAudit: "by=health from=" + abiX + " to=" + abiY + " slot=B",
		},
		{
			name:      "a target slot with no committed override clears the arm",
			entries:   []string{bootenv.KeyABI + "=" + abiX},
			running:   abiX,
			wantBlock: map[string]string{bootenv.KeyRejected: abiX},
			wantAudit: "by=health from=" + abiX + " to=none slot=B",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, tc.entries...)
			w.publish(abiX)
			w.runningIs(tc.running)
			w.writePrestate()

			require.NoError(t, w.undo())
			assert.Equal(t, tc.wantBlock, w.block())
			assert.Equal(t, tc.wantAudit, w.audit())
			assert.Empty(t, w.rejected(), "the rootfs rolled back, not the kernel bytes")
			assert.ElementsMatch(t, []string{abiX}, w.published(),
				"the relay's consumer owns the records")
			assert.True(t, w.prestateExists())
			assert.Equal(t, 1, w.locks, "one verdict, one write")
		})
	}
}

func TestHUPReject_AnEmptyArmWritesNeither(t *testing.T) {
	w := newWorld(t, bootenv.KeyCommitted(bootenv.SlotB)+"="+abiY)

	require.NoError(t, w.undo())
	assert.Equal(t, map[string]string{bootenv.KeyCommitted(bootenv.SlotB): abiY}, w.block())
	assert.Empty(t, w.audit())
	assert.Zero(t, w.locks)
}

func TestHUPReject_NoBlockExitsZero(t *testing.T) {
	w := newWorld(t, bootenv.KeyABI+"="+abiX)
	require.NoError(t, os.Remove(w.blockPath))

	assert.NoError(t, w.undo())
	assert.Empty(t, w.audit())
}

// Writing the wrong slot's committed value is how a proven kernel gets
// retired, so a slot that cannot be named refuses rather than guesses.
func TestHUP_RefusesAnUnnameableSlot(t *testing.T) {
	for name, run := range map[string]func(*world) error{
		"commit": (*world).commit,
		"reject": (*world).undo,
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, bootenv.KeyABI+"="+abiX)
			w.runningIs(abiX)
			w.labels(map[string]string{"resin-rootA": "/dev/nowhere"})

			assert.Error(t, run(w))
			assert.Equal(t, map[string]string{bootenv.KeyABI: abiX}, w.block())
			assert.Empty(t, w.audit(), "nothing is recorded against a slot that cannot be named")
		})
	}
}
