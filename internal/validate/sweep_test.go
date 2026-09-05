package validate

import (
	"errors"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A device with no override record never reads the container store.
func TestSweep_AnEmptyRecordedSetNeverReadsTheStore(t *testing.T) {
	w := newWorld(t, "upgrade_available=0")

	require.NoError(t, w.run())
	assert.Zero(t, w.claimCall)
}

func TestSweep_CollectsWhatNothingClaims(t *testing.T) {
	tests := []struct {
		name          string
		entries       []string
		published     []string
		claims        []string
		claimErr      error
		wantBlock     map[string]string
		wantPublished []string
	}{
		{
			name:          "a dangling link nothing claims",
			published:     []string{abiX},
			wantBlock:     map[string]string{},
			wantPublished: nil,
		},
		{
			name:          "an ABI a live container claims stays whole",
			entries:       []string{bootenv.KeyABI + "=" + abiX, bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiX},
			published:     []string{abiX},
			claims:        []string{abiX},
			wantBlock:     map[string]string{bootenv.KeyABI: abiX, bootenv.KeyCommitted(bootenv.SlotA): abiX},
			wantPublished: []string{abiX},
		},
		{
			// Containers exist and none claims an ABI: the genuinely empty
			// claim set, and the case the sweep exists for.
			name:          "no claimant at all collects everything recorded",
			entries:       []string{bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiX, bootenv.KeyCommitted(bootenv.SlotB) + "=" + abiY},
			published:     []string{abiX, abiY, abiZ},
			wantBlock:     map[string]string{},
			wantPublished: nil,
		},
		{
			// Any error, sentinel or not, means "do not sweep". Reading it as
			// an empty claim set would forget every ABI on a data partition
			// that is not yet populated.
			name:          "cannot tell leaves the records alone",
			entries:       []string{bootenv.KeyABI + "=" + abiX, bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiX},
			published:     []string{abiX},
			claimErr:      errors.New("cannot determine the deployed kernel ABI claims"),
			wantBlock:     map[string]string{bootenv.KeyABI: abiX, bootenv.KeyCommitted(bootenv.SlotA): abiX},
			wantPublished: []string{abiX},
		},
		{
			name:          "only the slot that names the unclaimed ABI is cleared",
			entries:       []string{bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiX, bootenv.KeyCommitted(bootenv.SlotB) + "=" + abiY},
			published:     []string{abiX, abiY},
			claims:        []string{abiY},
			wantBlock:     map[string]string{bootenv.KeyCommitted(bootenv.SlotB): abiY},
			wantPublished: []string{abiY},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, tc.entries...)
			w.publish(tc.published...)
			w.claims, w.claimErr = tc.claims, tc.claimErr

			require.NoError(t, w.run())
			assert.Equal(t, tc.wantBlock, w.block())
			assert.ElementsMatch(t, tc.wantPublished, w.published())
		})
	}
}

// The recorded set is read before any claim query and never recomputed: a
// container counts as a claimant from creation, so a deploy landing mid-sweep
// writes records this sweep must not judge.
func TestSweep_RecordsWrittenAfterTheClaimQueryAreNotJudged(t *testing.T) {
	w := newWorld(t,
		bootenv.KeyABI+"="+abiX,
		bootenv.KeyCommitted(bootenv.SlotA)+"="+abiX)
	w.publish(abiX)
	w.claims = []string{abiX}
	w.onClaim = func() { w.publish(abiZ) }

	require.NoError(t, w.run())
	assert.Equal(t, 1, w.claimCall)
	assert.ElementsMatch(t, []string{abiX, abiZ}, w.published(),
		"a record the claims snapshot could not have covered must survive")
}

// A redeploy reuses the ABI, so it is in the recorded set already and the
// ordering above does not cover it. The lock the runtime's create also takes
// is what does.
func TestSweep_TheClaimsAreAnsweredUnderTheLock(t *testing.T) {
	w := newWorld(t, bootenv.KeyABI+"="+abiX)
	w.publish(abiX)
	w.claims = []string{abiX}
	w.onClaim = func() { require.True(t, w.lockHeld, "the claims must be answered under the lock") }

	require.NoError(t, w.run())
	assert.Equal(t, 1, w.claimCall, "one query, and it is the one the writes act on")
	assert.Equal(t, map[string]string{bootenv.KeyABI: abiX}, w.block())
	assert.ElementsMatch(t, []string{abiX}, w.published())
}

// The sweep clears an arm without restoring it, since it runs before the slot
// can be named. The empty-arm row is what puts the proven override back.
func TestSweep_ForgettingTheArmRestoresTheSlotsProvenOverride(t *testing.T) {
	w := newWorld(t,
		bootenv.KeyABI+"="+abiY,
		bootenv.KeyTrial+"=2",
		bootenv.KeyCommitted(bootenv.SlotA)+"="+abiX)
	w.publish(abiX, abiY)
	w.claims = []string{abiX}
	w.runningIs(abiY)

	require.NoError(t, w.run())
	assert.Equal(t, map[string]string{
		bootenv.KeyABI:                      abiX,
		bootenv.KeyCommitted(bootenv.SlotA): abiX,
	}, w.block())
	assert.ElementsMatch(t, []string{abiX}, w.published())
	assert.Empty(t, w.rejected(), "nothing was judged")
}

// The count belongs to the arm, so a sweep of another slot's leftover must
// not hand a pending arm a fresh boot budget.
func TestSweep_TheTrialCountFollowsTheArm(t *testing.T) {
	t.Run("forgetting an ABI that is not the arm leaves the count", func(t *testing.T) {
		w := newWorld(t,
			bootenv.KeyABI+"="+abiX,
			bootenv.KeyTrial+"=2",
			bootenv.KeyCommitted(bootenv.SlotB)+"="+abiY)
		w.publish(abiX, abiY)
		w.claims = []string{abiX}

		require.NoError(t, w.run())
		assert.Equal(t, map[string]string{
			bootenv.KeyABI: abiX, bootenv.KeyTrial: "2",
		}, w.block())
	})

	t.Run("forgetting the armed one clears it", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX, bootenv.KeyTrial+"=2")
		w.publish(abiX)

		require.NoError(t, w.run())
		assert.Empty(t, w.block())
	})
}

// The prestate stands for an open window, so it follows the arm the same way
// the count does.
func TestSweep_TheHealthPrestateFollowsTheArm(t *testing.T) {
	t.Run("forgetting an ABI that is not the arm leaves the prestate", func(t *testing.T) {
		w := newWorld(t,
			bootenv.KeyABI+"="+abiX,
			bootenv.KeyCommitted(bootenv.SlotB)+"="+abiY)
		w.publish(abiX, abiY)
		w.claims = []string{abiX}
		w.writePrestate()

		require.NoError(t, w.run())
		assert.True(t, w.prestateExists(), "the arm's window is still open")
	})

	t.Run("forgetting the armed one closes its window", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX)
		w.publish(abiX)
		w.writePrestate()

		require.NoError(t, w.run())
		assert.False(t, w.prestateExists())
	})
}

// Why the sweep runs first: read before it, an armed value equal to a
// committed one exits as an ordinary boot and never looks again, leaving both
// records and the published kernel for the device's life.
func TestRun_SweepRunsBeforeTheArmIsRead(t *testing.T) {
	w := newWorld(t,
		bootenv.KeyABI+"="+abiX,
		bootenv.KeyCommitted(bootenv.SlotA)+"="+abiX)
	w.publish(abiX)

	require.NoError(t, w.run())
	assert.Empty(t, w.block())
	assert.Empty(t, w.published())
	assert.Equal(t, 1, w.locks, "the sweep is one locked write")
}
