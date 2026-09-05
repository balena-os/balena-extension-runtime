package validate

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/balena-os/balena-extension-runtime/internal/override"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A device whose bootloader environment is not a grub block has no override
// axis, which is not a defect. A block that is present and unparseable is.
func TestRun_BlockStates(t *testing.T) {
	t.Run("absent exits zero", func(t *testing.T) {
		w := newWorld(t)
		require.NoError(t, os.Remove(w.blockPath))

		assert.NoError(t, w.run())
		assert.Zero(t, w.locks)
	})

	t.Run("unparseable fails having written nothing", func(t *testing.T) {
		w := newWorld(t)
		require.NoError(t, os.WriteFile(w.blockPath, make([]byte, blockSize), 0o644))
		before, err := os.ReadFile(w.blockPath)
		require.NoError(t, err)

		assert.Error(t, w.run())
		after, err := os.ReadFile(w.blockPath)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})
}

// The relay is only ever written inside an update window, so it is consumed
// above the gate. Nothing below it runs.
func TestRun_TheRelayIsConsumedAboveTheUpdateGate(t *testing.T) {
	w := newWorld(t,
		bootenv.KeyUpgradeAvailable+"=1",
		bootenv.KeyRejected+"="+abiX,
		bootenv.KeyABI+"="+abiY,
		bootenv.KeyTrial+"=2",
		bootenv.KeyCommitted(bootenv.SlotA)+"="+abiX)
	w.publish(abiX, abiY)
	w.writePrestate()

	require.NoError(t, w.run())
	assert.Equal(t, map[string]string{
		bootenv.KeyUpgradeAvailable: "1",
		bootenv.KeyABI:              abiY,
	}, w.block())
	assert.ElementsMatch(t, []string{abiY}, w.published())
	assert.False(t, w.prestateExists(), "the relay closes the window")
	assert.Zero(t, w.claimCall, "the sweep is below the gate")
}

func TestRun_AnUpdateInProgressLeavesTheOverrideAlone(t *testing.T) {
	w := newWorld(t, bootenv.KeyUpgradeAvailable+"=1", bootenv.KeyABI+"="+abiX)
	w.publish(abiX)
	w.runningIs(abiX)

	require.NoError(t, w.run())
	assert.Equal(t, map[string]string{
		bootenv.KeyUpgradeAvailable: "1",
		bootenv.KeyABI:              abiX,
	}, w.block())
	assert.Zero(t, w.claimCall)
}

func TestRun_StateMachine(t *testing.T) {
	tests := []struct {
		name          string
		entries       []string
		published     []string
		claims        []string
		running       string
		health        []error
		wantBlock     map[string]string
		wantPublished []string
		wantRejected  string
		wantAudit     string
		wantPrestate  bool
		wantReboots   int
	}{
		{
			name:          "an ordinary boot writes nothing",
			entries:       []string{bootenv.KeyABI + "=" + abiX, bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiX},
			published:     []string{abiX},
			claims:        []string{abiX},
			running:       abiX,
			wantBlock:     map[string]string{bootenv.KeyABI: abiX, bootenv.KeyCommitted(bootenv.SlotA): abiX},
			wantPublished: []string{abiX},
			wantPrestate:  true,
		},
		{
			// A swept arm, not a release that dropped its override.
			name:          "an empty arm beside a proven override restores it",
			entries:       []string{bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiX},
			published:     []string{abiX},
			claims:        []string{abiX},
			wantBlock:     map[string]string{bootenv.KeyABI: abiX, bootenv.KeyCommitted(bootenv.SlotA): abiX},
			wantPublished: []string{abiX},
			wantPrestate:  true,
		},
		{
			// The sweep above kept it, so it is claimed and a missing kernel
			// is a defect rather than a verdict. Nothing is recorded.
			name:      "a claimed arm with no published kernel is forgotten",
			entries:   []string{bootenv.KeyABI + "=" + abiX, bootenv.KeyTrial + "=1"},
			claims:    []string{abiX},
			wantBlock: map[string]string{},
		},
		{
			name: "a spent boot budget is the verdict",
			entries: []string{
				bootenv.KeyABI + "=" + abiX,
				bootenv.KeyTrial + "=3",
				bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiY,
			},
			published:     []string{abiX, abiY},
			claims:        []string{abiX, abiY},
			running:       abiY,
			wantBlock:     map[string]string{bootenv.KeyCommitted(bootenv.SlotA): abiY, bootenv.KeyABI: abiY},
			wantPublished: []string{abiY},
			wantRejected:  abiX + "\n",
			wantAudit:     "by=boots from=" + abiX + " to=" + abiY + " slot=A boots=3",
		},
		{
			name:          "an arm that has not taken effect yet is left pending",
			entries:       []string{bootenv.KeyABI + "=" + abiX, bootenv.KeyTrial + "=1"},
			published:     []string{abiX},
			claims:        []string{abiX},
			wantBlock:     map[string]string{bootenv.KeyABI: abiX, bootenv.KeyTrial: "1"},
			wantPublished: []string{abiX},
			wantPrestate:  true,
		},
		{
			name:          "healthy checks commit the running kernel",
			entries:       []string{bootenv.KeyABI + "=" + abiX, bootenv.KeyTrial + "=1"},
			published:     []string{abiX},
			claims:        []string{abiX},
			running:       abiX,
			health:        []error{nil},
			wantBlock:     map[string]string{bootenv.KeyABI: abiX, bootenv.KeyCommitted(bootenv.SlotA): abiX},
			wantPublished: []string{abiX},
		},
		{
			name: "failed checks reject and reboot",
			entries: []string{
				bootenv.KeyABI + "=" + abiX,
				bootenv.KeyTrial + "=1",
				bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiY,
			},
			published:     []string{abiX, abiY},
			claims:        []string{abiX, abiY},
			running:       abiX,
			health:        []error{checkFailed(t)},
			wantBlock:     map[string]string{bootenv.KeyCommitted(bootenv.SlotA): abiY, bootenv.KeyABI: abiY},
			wantPublished: []string{abiY},
			wantRejected:  abiX + "\n",
			wantAudit:     "by=health from=" + abiX + " to=" + abiY + " slot=A",
			wantReboots:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, tc.entries...)
			w.publish(tc.published...)
			w.claims = tc.claims
			w.runningIs(tc.running)
			if tc.health != nil {
				w.health = tc.health
			}
			w.writePrestate()

			require.NoError(t, w.run())
			assert.Equal(t, tc.wantBlock, w.block())
			assert.ElementsMatch(t, tc.wantPublished, w.published())
			assert.Equal(t, tc.wantRejected, w.rejected())
			assert.Equal(t, tc.wantAudit, w.audit())
			assert.Equal(t, tc.wantPrestate, w.prestateExists())
			assert.Equal(t, tc.wantReboots, w.rebooted)
		})
	}
}

// The rejection record is permanent, so only a check that ran and said no
// reaches it. A check that never ran is a machine condition.
func TestRun_AHealthcheckThatNeverRanIsNotAVerdict(t *testing.T) {
	w := newWorld(t, bootenv.KeyABI+"="+abiX, bootenv.KeyTrial+"=1")
	w.publish(abiX)
	w.claims = []string{abiX}
	w.runningIs(abiX)
	w.writePrestate()
	w.health = []error{assert.AnError}

	assert.Error(t, w.run())
	assert.Equal(t, 1, w.healthCalls, "an exec that failed is not retried")
	assert.Equal(t, map[string]string{
		bootenv.KeyABI: abiX, bootenv.KeyTrial: "1",
	}, w.block(), "the arm is left for the boot budget to bound")
	assert.Empty(t, w.rejected())
	assert.Empty(t, w.audit())
	assert.True(t, w.prestateExists())
	assert.Zero(t, w.rebooted)
}

// A spent count is read only on the row where the arm did not take effect.
func TestRun_TheTrialLimit(t *testing.T) {
	tests := []struct {
		count      string
		wantReject bool
	}{
		{count: "", wantReject: false},
		{count: "0", wantReject: false},
		{count: strconv.Itoa(trialLimit - 1), wantReject: false},
		{count: strconv.Itoa(trialLimit), wantReject: true},
		{count: strconv.Itoa(trialLimit + 1), wantReject: true},
		{count: "not-a-number", wantReject: false},
	}

	for _, tc := range tests {
		t.Run("count "+tc.count, func(t *testing.T) {
			entries := []string{bootenv.KeyABI + "=" + abiX}
			if tc.count != "" {
				entries = append(entries, bootenv.KeyTrial+"="+tc.count)
			}
			w := newWorld(t, entries...)
			w.publish(abiX)
			w.claims = []string{abiX}

			require.NoError(t, w.run())
			if tc.wantReject {
				assert.Equal(t, abiX+"\n", w.rejected())
				assert.Empty(t, w.block())
			} else {
				assert.Empty(t, w.rejected())
				assert.Equal(t, abiX, w.block()[bootenv.KeyABI])
			}
		})
	}
}

// helios can still land a deploy during the trial, and an arm that moved
// belongs to its own boot rather than to this verdict.
func TestRun_AMovedArm(t *testing.T) {
	t.Run("a commit writes nothing and leaves the new window its prestate", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX, bootenv.KeyTrial+"=1")
		w.publish(abiX, abiY)
		w.claims = []string{abiX, abiY}
		w.runningIs(abiX)
		w.writePrestate()
		w.health = []error{nil}
		// The deploy lands while the healthchecks run.
		runHealthcheck = func(context.Context) error {
			w.writeBlock(bootenv.KeyABI + "=" + abiY)
			return nil
		}

		require.NoError(t, w.run())
		assert.Equal(t, map[string]string{bootenv.KeyABI: abiY}, w.block())
		assert.True(t, w.prestateExists(), "activation rewrote it when it armed the new window")
	})

	t.Run("a rejection still records and still reboots", func(t *testing.T) {
		w := newWorld(t, bootenv.KeyABI+"="+abiX, bootenv.KeyTrial+"=1")
		w.publish(abiX, abiY)
		w.claims = []string{abiX, abiY}
		w.runningIs(abiX)
		w.writePrestate()
		runHealthcheck = func(context.Context) error {
			w.writeBlock(bootenv.KeyABI + "=" + abiY)
			return checkFailed(t)
		}

		require.NoError(t, w.run())
		assert.Equal(t, map[string]string{bootenv.KeyABI: abiY}, w.block())
		assert.Equal(t, abiX+"\n", w.rejected(), "the judged kernel bytes failed either way")
		assert.Equal(t, "by=health from="+abiX+" to=none slot=A", w.audit())
		assert.ElementsMatch(t, []string{abiY}, w.published())
		assert.True(t, w.prestateExists())
		assert.Equal(t, 1, w.rebooted, "the running kernel is the rejected one")
	})
}

// Each verdict's intermediate states have to converge on the next run.
func TestRun_CrashPoints(t *testing.T) {
	tests := []struct {
		name          string
		entries       []string
		published     []string
		claims        []string
		running       string
		rejected      string
		wantBlock     map[string]string
		wantPublished []string
	}{
		{
			name:          "forget wrote the block but not the unlink",
			published:     []string{abiX},
			wantBlock:     map[string]string{},
			wantPublished: nil,
		},
		{
			name:          "the relay cleared the block but not the unlink",
			published:     []string{abiX},
			wantBlock:     map[string]string{},
			wantPublished: nil,
		},
		{
			// The record is what refuses the arm when helios redeploys, so it
			// is written first and a replay must not double it.
			name: "reject wrote the records but not the block",
			entries: []string{
				bootenv.KeyABI + "=" + abiX,
				bootenv.KeyTrial + "=3",
				bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiY,
			},
			published:     []string{abiX, abiY},
			claims:        []string{abiX, abiY},
			running:       abiY,
			rejected:      abiX + "\n",
			wantBlock:     map[string]string{bootenv.KeyCommitted(bootenv.SlotA): abiY, bootenv.KeyABI: abiY},
			wantPublished: []string{abiY},
		},
		{
			name: "reject wrote the block but not the unlink",
			entries: []string{
				bootenv.KeyABI + "=" + abiY,
				bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiY,
			},
			published:     []string{abiX, abiY},
			claims:        []string{abiY},
			running:       abiY,
			rejected:      abiX + "\n",
			wantBlock:     map[string]string{bootenv.KeyABI: abiY, bootenv.KeyCommitted(bootenv.SlotA): abiY},
			wantPublished: []string{abiY},
		},
		{
			// The next arm replaces the prestate through the rename
			// activation already uses, so a stale one costs nothing.
			name: "commit wrote the block but did not remove the prestate",
			entries: []string{
				bootenv.KeyABI + "=" + abiX,
				bootenv.KeyCommitted(bootenv.SlotA) + "=" + abiX,
			},
			published:     []string{abiX},
			claims:        []string{abiX},
			running:       abiX,
			wantBlock:     map[string]string{bootenv.KeyABI: abiX, bootenv.KeyCommitted(bootenv.SlotA): abiX},
			wantPublished: []string{abiX},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, tc.entries...)
			w.publish(tc.published...)
			w.claims = tc.claims
			w.runningIs(tc.running)
			w.writePrestate()
			if tc.rejected != "" {
				require.NoError(t, os.WriteFile(override.RejectedPath(), []byte(tc.rejected), 0o644))
			}

			require.NoError(t, w.run())
			assert.Equal(t, tc.wantBlock, w.block())
			assert.ElementsMatch(t, tc.wantPublished, w.published())
			if tc.rejected != "" {
				assert.Equal(t, tc.rejected, w.rejected(), "a replay must not double the record")
			}
		})
	}
}

// The wait is up to sixteen minutes, and holding the lock there would stall
// the cleanup unit for the length of every trial.
func TestRun_TheLockIsNotHeldAcrossTheWaits(t *testing.T) {
	w := newWorld(t, bootenv.KeyABI+"="+abiX, bootenv.KeyTrial+"=1")
	w.publish(abiX)
	w.claims = []string{abiX}
	w.runningIs(abiX)
	w.health = []error{checkFailed(t), checkFailed(t), nil}

	require.NoError(t, w.run())
	assert.Equal(t, []time.Duration{time.Minute, time.Minute, time.Minute}, w.waits,
		"one settle then one retry per failed attempt")
	assert.Equal(t, 2, w.locks, "only the sweep and the verdict take it")
}
