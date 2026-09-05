package bootenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	abiX = "aaaa"
	abiY = "bbbb"
	abiZ = "cccc"
)

// seedEntries lays down a block holding exactly these lines and points the
// package at it.
func seedEntries(t *testing.T, entries ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(signature)
	for _, e := range entries {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	require.Less(t, b.Len(), defaultBlockSize)

	block := make([]byte, defaultBlockSize)
	copy(block, b.String())
	for i := b.Len(); i < defaultBlockSize; i++ {
		block[i] = pad
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "bootenv")
	require.NoError(t, os.WriteFile(path, block, 0o644))
	t.Cleanup(SetBootMount(dir))
	return path
}

func readBack(t *testing.T) map[string]string {
	t.Helper()
	env, err := Read()
	require.NoError(t, err)
	out := map[string]string{}
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		out[k] = v
	}
	return out
}

// countUpdates makes every verdict's single read-modify-write countable.
func countUpdates(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := updateBlock
	updateBlock = func(fn func(*Env) error) error {
		calls++
		return prev(fn)
	}
	t.Cleanup(func() { updateBlock = prev })
	return &calls
}

func TestSlot_OtherIsTheComplement(t *testing.T) {
	assert.Equal(t, SlotB, SlotA.Other())
	assert.Equal(t, SlotA, SlotB.Other())
	assert.Equal(t, "kernel_override_abi_committed_A", KeyCommitted(SlotA))
	assert.Equal(t, "kernel_override_abi_committed_B", KeyCommitted(SlotB))
}

func TestRead_AbsentBlockIsNotADefect(t *testing.T) {
	seedEntries(t)
	require.NoError(t, os.Remove(Path()))

	_, err := Read()
	assert.ErrorIs(t, err, ErrNoBlock)
	assert.ErrorIs(t, Update(func(*Env) error { return nil }), ErrNoBlock)
}

func TestRead_UnparseableBlockIsADefect(t *testing.T) {
	path := seedEntries(t)
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", defaultBlockSize)), 0o644))

	_, err := Read()
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoBlock)
}

func TestRead_LeavesTheBlockByteIdentical(t *testing.T) {
	path := seedEntries(t, KeyABI+"="+abiX, KeyTrial+"=2")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = Read()
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestRead_UnmountedBootPartition(t *testing.T) {
	seedEntries(t)
	prev := isMounted
	isMounted = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { isMounted = prev })

	_, err := Read()
	assert.ErrorIs(t, err, ErrNotMounted)
}

func TestForget(t *testing.T) {
	tests := []struct {
		name       string
		entries    []string
		abis       []string
		armCleared bool
		want       map[string]string
	}{
		{
			name:       "the armed ABI takes the trial count with it",
			entries:    []string{KeyABI + "=" + abiX, KeyTrial + "=2", KeyCommitted(SlotA) + "=" + abiX},
			abis:       []string{abiX},
			armCleared: true,
			want:       map[string]string{},
		},
		{
			// An unconditional clear would hand the pending arm a fresh boot
			// budget and postpone the spent-count rejection.
			name:       "another slot's leftover leaves the pending arm its budget",
			entries:    []string{KeyABI + "=" + abiX, KeyTrial + "=2", KeyCommitted(SlotB) + "=" + abiY},
			abis:       []string{abiY},
			armCleared: false,
			want:       map[string]string{KeyABI: abiX, KeyTrial: "2"},
		},
		{
			name:       "the erasure spans both slots",
			entries:    []string{KeyCommitted(SlotA) + "=" + abiX, KeyCommitted(SlotB) + "=" + abiX},
			abis:       []string{abiX},
			armCleared: false,
			want:       map[string]string{},
		},
		{
			name:       "a slot that committed something else is left alone",
			entries:    []string{KeyCommitted(SlotA) + "=" + abiX, KeyCommitted(SlotB) + "=" + abiY},
			abis:       []string{abiX},
			armCleared: false,
			want:       map[string]string{KeyCommitted(SlotB): abiY},
		},
		{
			name:       "several ABIs at once",
			entries:    []string{KeyABI + "=" + abiX, KeyCommitted(SlotA) + "=" + abiY, KeyCommitted(SlotB) + "=" + abiZ},
			abis:       []string{abiX, abiY, abiZ},
			armCleared: true,
			want:       map[string]string{},
		},
		{
			name:       "keys this package does not own survive",
			entries:    []string{"upgrade_available=0", KeyABI + "=" + abiX},
			abis:       []string{abiX},
			armCleared: true,
			want:       map[string]string{"upgrade_available": "0"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seedEntries(t, tc.entries...)
			calls := countUpdates(t)

			armCleared, err := Forget(tc.abis)
			require.NoError(t, err)
			assert.Equal(t, tc.armCleared, armCleared)
			assert.Equal(t, tc.want, readBack(t))
			assert.Equal(t, 1, *calls, "a sweep of N ABIs is one write")
		})
	}
}

func TestForget_AnEmptySetNeverOpensTheBlock(t *testing.T) {
	seedEntries(t)
	require.NoError(t, os.Remove(Path()))

	armCleared, err := Forget([]string{"", ""})
	require.NoError(t, err)
	assert.False(t, armCleared)
}

// The relay is a window close, so the count goes whatever the arm names.
func TestConsumeRelay_ClearsTheBudgetAndTheRelay(t *testing.T) {
	seedEntries(t,
		KeyABI+"="+abiY, KeyTrial+"=2",
		KeyCommitted(SlotA)+"="+abiX, KeyCommitted(SlotB)+"="+abiY,
		KeyRejected+"="+abiX)
	calls := countUpdates(t)

	require.NoError(t, ConsumeRelay(abiX))
	assert.Equal(t, map[string]string{
		KeyABI:              abiY,
		KeyCommitted(SlotB): abiY,
	}, readBack(t))
	assert.Equal(t, 1, *calls)
}

func TestCommit(t *testing.T) {
	tests := []struct {
		name      string
		entries   []string
		abi       string
		slot      Slot
		wantWrote bool
		want      map[string]string
	}{
		{
			name:      "a healthy trial",
			entries:   []string{KeyABI + "=" + abiX, KeyTrial + "=2"},
			abi:       abiX,
			slot:      SlotA,
			wantWrote: true,
			want:      map[string]string{KeyABI: abiX, KeyCommitted(SlotA): abiX},
		},
		{
			name:      "the window moved",
			entries:   []string{KeyABI + "=" + abiY, KeyTrial + "=1"},
			abi:       abiX,
			slot:      SlotA,
			wantWrote: false,
			want:      map[string]string{KeyABI: abiY, KeyTrial: "1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := seedEntries(t, tc.entries...)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			calls := countUpdates(t)

			wrote, err := Commit(tc.abi, tc.slot)
			require.NoError(t, err)
			assert.Equal(t, tc.wantWrote, wrote)
			assert.Equal(t, tc.want, readBack(t))
			assert.Equal(t, 1, *calls, "a verdict is one read-modify-write")

			if !tc.wantWrote {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after, "a moved window rewrites nothing")
			}
		})
	}
}

// Forget clears an arm without restoring it, so a swept arm beside a still
// claimed committed value has to come back.
func TestRestore(t *testing.T) {
	tests := []struct {
		name      string
		entries   []string
		wantWrote bool
		want      map[string]string
	}{
		{
			name:      "a swept arm comes back as the slot's proven override",
			entries:   []string{KeyCommitted(SlotA) + "=" + abiX},
			wantWrote: true,
			want:      map[string]string{KeyABI: abiX, KeyCommitted(SlotA): abiX},
		},
		{
			// A deploy armed one meanwhile, and that arm owns the next boot.
			name:      "an arm that is already set is left alone",
			entries:   []string{KeyABI + "=" + abiY, KeyCommitted(SlotA) + "=" + abiX},
			wantWrote: false,
			want:      map[string]string{KeyABI: abiY, KeyCommitted(SlotA): abiX},
		},
		{
			name:      "a slot that has proven nothing stays stock",
			entries:   []string{KeyCommitted(SlotB) + "=" + abiY},
			wantWrote: false,
			want:      map[string]string{KeyCommitted(SlotB): abiY},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := seedEntries(t, tc.entries...)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			calls := countUpdates(t)

			wrote, err := Restore(SlotA)
			require.NoError(t, err)
			assert.Equal(t, tc.wantWrote, wrote)
			assert.Equal(t, tc.want, readBack(t))
			assert.Equal(t, 1, *calls, "a restore is one read-modify-write")

			if !tc.wantWrote {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after, "no restore rewrites nothing")
			}
		})
	}
}

func TestReject(t *testing.T) {
	tests := []struct {
		name      string
		entries   []string
		slot      Slot
		wantWrote bool
		want      map[string]string
	}{
		{
			// The window extension-rollback:217-223 describes: the arm and
			// the restore are one write, so no state shows one without the
			// other.
			name:      "the slot's proven override comes back",
			entries:   []string{KeyABI + "=" + abiX, KeyTrial + "=3", KeyCommitted(SlotA) + "=" + abiY},
			slot:      SlotA,
			wantWrote: true,
			want:      map[string]string{KeyABI: abiY, KeyCommitted(SlotA): abiY},
		},
		{
			name:      "a slot with no committed override runs stock",
			entries:   []string{KeyABI + "=" + abiX, KeyTrial + "=3"},
			slot:      SlotA,
			wantWrote: true,
			want:      map[string]string{},
		},
		{
			name:      "the rejected ABI goes from both slots",
			entries:   []string{KeyABI + "=" + abiX, KeyCommitted(SlotA) + "=" + abiX, KeyCommitted(SlotB) + "=" + abiX},
			slot:      SlotA,
			wantWrote: true,
			want:      map[string]string{},
		},
		{
			name:      "the window moved",
			entries:   []string{KeyABI + "=" + abiY, KeyTrial + "=3", KeyCommitted(SlotA) + "=" + abiZ},
			slot:      SlotA,
			wantWrote: false,
			want:      map[string]string{KeyABI: abiY, KeyTrial: "3", KeyCommitted(SlotA): abiZ},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := seedEntries(t, tc.entries...)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			calls := countUpdates(t)

			wrote, err := Reject(abiX, tc.slot)
			require.NoError(t, err)
			assert.Equal(t, tc.wantWrote, wrote)
			assert.Equal(t, tc.want, readBack(t))
			assert.Equal(t, 1, *calls, "a verdict is one read-modify-write")

			if !tc.wantWrote {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after)
			}
		})
	}
}

func TestHUPReject(t *testing.T) {
	tests := []struct {
		name        string
		entries     []string
		running     string
		wantRelayed bool
		want        map[string]string
	}{
		{
			name:        "the arm took effect and differs from the target's",
			entries:     []string{KeyABI + "=" + abiX, KeyTrial + "=1", KeyCommitted(SlotB) + "=" + abiY},
			running:     abiX,
			wantRelayed: true,
			want: map[string]string{
				KeyABI: abiY, KeyTrial: "1",
				KeyCommitted(SlotB): abiY, KeyRejected: abiX,
			},
		},
		{
			// A redundant rollback: relaying would forget a proven ABI
			// everywhere and put proven kernel bytes through a trial again.
			name:        "the arm is already the target's proven override",
			entries:     []string{KeyABI + "=" + abiX, KeyCommitted(SlotB) + "=" + abiX},
			running:     abiX,
			wantRelayed: false,
			want:        map[string]string{KeyABI: abiX, KeyCommitted(SlotB): abiX},
		},
		{
			name:        "the arm never took effect",
			entries:     []string{KeyABI + "=" + abiX, KeyCommitted(SlotB) + "=" + abiY},
			running:     "",
			wantRelayed: false,
			want:        map[string]string{KeyABI: abiY, KeyCommitted(SlotB): abiY},
		},
		{
			name:        "an empty arm",
			entries:     []string{KeyCommitted(SlotB) + "=" + abiY},
			running:     "",
			wantRelayed: false,
			want:        map[string]string{KeyABI: abiY, KeyCommitted(SlotB): abiY},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seedEntries(t, tc.entries...)
			calls := countUpdates(t)

			relayed, err := HUPReject(SlotB, tc.running)
			require.NoError(t, err)
			assert.Equal(t, tc.wantRelayed, relayed)
			assert.Equal(t, tc.want, readBack(t))
			assert.Equal(t, 1, *calls)
		})
	}
}
