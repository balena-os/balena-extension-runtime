package bootenv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	require.Len(t, data, defaultBlockSize, "fixture is not a grub environment block")
	return data
}

func TestParse_FreshBlockHasNoEntries(t *testing.T) {
	env, err := Parse(readFixture(t, "created.bootenv"))
	require.NoError(t, err)
	assert.Empty(t, env.Keys())
}

func TestParse_ReadsEntriesInOrder(t *testing.T) {
	env, err := Parse(readFixture(t, "armed.bootenv"))
	require.NoError(t, err)

	assert.Equal(t, []string{"kernel_override_abi", "kernel_override_trial", "kernel_override_abi_committed_A"}, env.Keys())

	abi, ok := env.Get("kernel_override_abi")
	assert.True(t, ok)
	assert.Equal(t, strings.Repeat("1", 64), abi)

	// A key set to the empty string is set, not absent. rollback-health
	// writes exactly that to mean "this slot is known good on stock".
	committed, ok := env.Get("kernel_override_abi_committed_A")
	assert.True(t, ok)
	assert.Equal(t, "", committed)
}

func TestParse_RejectsAForeignBlock(t *testing.T) {
	_, err := Parse([]byte(strings.Repeat("x", defaultBlockSize)))
	assert.Error(t, err)
}

func TestParse_RejectsABlockShorterThanTheSignature(t *testing.T) {
	_, err := Parse([]byte(signature[:len(signature)-1]))
	assert.Error(t, err)
}

// A block keeps its own size, not grub-editenv's default.
func TestMarshal_KeepsANonDefaultBlockSize(t *testing.T) {
	const size = defaultBlockSize * 2
	original := append(readFixture(t, "armed.bootenv"),
		[]byte(strings.Repeat("#", size-defaultBlockSize))...)

	env, err := Parse(original)
	require.NoError(t, err)

	rendered, err := env.Marshal()
	require.NoError(t, err)
	assert.Len(t, rendered, size)
	assert.Equal(t, original, rendered)

	require.NoError(t, env.Set("added", "value"))
	rendered, err = env.Marshal()
	require.NoError(t, err)
	assert.Len(t, rendered, size, "a write must not resize the block")
}

func TestParse_RejectsAnOversizedBlock(t *testing.T) {
	_, err := Parse(make([]byte, maxBlockSize+1))
	assert.Error(t, err)
}

// A block the package renders has to be byte-identical to what grub-editenv
// would leave behind, or the next writer of either kind reads a file it does
// not recognise.
func TestMarshal_RoundTripsEveryFixture(t *testing.T) {
	for _, name := range []string{"created.bootenv", "armed.bootenv", "packed.bootenv"} {
		t.Run(name, func(t *testing.T) {
			original := readFixture(t, name)
			env, err := Parse(original)
			require.NoError(t, err)
			rendered, err := env.Marshal()
			require.NoError(t, err)
			assert.Equal(t, original, rendered)
		})
	}
}

func TestSet_AppendsAndOverwrites(t *testing.T) {
	env, err := Parse(readFixture(t, "created.bootenv"))
	require.NoError(t, err)

	require.NoError(t, env.Set("a", "1"))
	require.NoError(t, env.Set("b", "2"))
	require.NoError(t, env.Set("a", "3"))

	assert.Equal(t, []string{"a", "b"}, env.Keys(), "overwriting must not move the key")
	v, _ := env.Get("a")
	assert.Equal(t, "3", v)
}

func TestUnset_RemovesTheKey(t *testing.T) {
	env, err := Parse(readFixture(t, "armed.bootenv"))
	require.NoError(t, err)

	env.Unset("kernel_override_trial")
	_, ok := env.Get("kernel_override_trial")
	assert.False(t, ok)
	assert.Equal(t, []string{"kernel_override_abi", "kernel_override_abi_committed_A"}, env.Keys())

	env.Unset("never_was_there")
	assert.Equal(t, []string{"kernel_override_abi", "kernel_override_abi_committed_A"}, env.Keys())
}

// The block is line-oriented with no escaping, so a newline or a key holding
// the separator would produce a file that parses back as something else.
func TestSet_RejectsUnrepresentableEntries(t *testing.T) {
	env, err := Parse(readFixture(t, "created.bootenv"))
	require.NoError(t, err)

	assert.Error(t, env.Set("", "v"))
	assert.Error(t, env.Set("a=b", "v"))
	assert.Error(t, env.Set("a\nb", "v"))
	assert.Error(t, env.Set("a", "v\nw"))
	assert.Error(t, env.Set("#a", "v"), "a key starting with # renders as a comment")
}

func TestMarshal_RefusesEntriesThatDoNotFit(t *testing.T) {
	env, err := Parse(readFixture(t, "packed.bootenv"))
	require.NoError(t, err)

	require.NoError(t, env.Set("one_key_too_many", strings.Repeat("x", 200)))
	_, err = env.Marshal()
	assert.Error(t, err, "the block is a fixed size; overflow must not truncate it")
}

// grub-editenv finds its free space by scanning back over trailing '#', so a
// block rendered without any is one it refuses to write to ever again. Entries
// that exactly fill the block are an overflow, not a tight fit.
func TestMarshal_AlwaysLeavesPadding(t *testing.T) {
	env, err := Parse(readFixture(t, "created.bootenv"))
	require.NoError(t, err)

	// One byte short of the block, so the entry ends on its last byte.
	free := defaultBlockSize - len(signature)
	require.NoError(t, env.Set("k", strings.Repeat("x", free-len("k=\n"))))
	_, err = env.Marshal()
	assert.Error(t, err, "a block rendered without padding rejects every later write")

	require.NoError(t, env.Set("k", strings.Repeat("x", free-len("k=\n")-1)))
	rendered, err := env.Marshal()
	require.NoError(t, err)
	assert.Equal(t, byte(pad), rendered[len(rendered)-1])
}

// The block on a device is what grub-editenv create leaves plus entries: a
// signature and padding, no other comment. One that does carry a comment still
// parses, and the comment is dropped rather than preserved.
func TestParse_DropsForeignCommentLines(t *testing.T) {
	const comment = "# WARNING: Do not edit this file by tools other than grub-editenv!!!\n"
	original := readFixture(t, "armed.bootenv")
	commented := []byte(signature + comment + string(original[len(signature):len(original)-len(comment)]))
	require.Len(t, commented, defaultBlockSize)

	env, err := Parse(commented)
	require.NoError(t, err)
	assert.Equal(t, []string{"kernel_override_abi", "kernel_override_trial", "kernel_override_abi_committed_A"}, env.Keys())

	rendered, err := env.Marshal()
	require.NoError(t, err)
	assert.Equal(t, original, rendered, "a comment carries no state and is not carried over")
}

// seedBlock points the package at a temporary boot mountpoint holding the
// given fixture, and satisfies the mount check.
func seedBlock(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootenv"), readFixture(t, fixture), 0o644))

	prevMount, prevMounted := bootMount, isMounted
	bootMount = dir
	isMounted = func(path string) (bool, error) { return path == dir, nil }
	t.Cleanup(func() { bootMount, isMounted = prevMount, prevMounted })
	return filepath.Join(dir, "bootenv")
}

// The arm and the trial reset are one write. Two writes would leave a window
// in which the block shows an armed override next to a count inherited from a
// window that already closed.
func TestArm_SetsTheABIAndClearsTheTrialInOneWrite(t *testing.T) {
	path := seedBlock(t, "armed.bootenv")
	abi := strings.Repeat("2", 64)

	require.NoError(t, Arm(abi))

	block, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, block, defaultBlockSize, "the block must keep its size")

	env, err := Parse(block)
	require.NoError(t, err)

	got, ok := env.Get(KeyABI)
	assert.True(t, ok)
	assert.Equal(t, abi, got)

	_, ok = env.Get(KeyTrial)
	assert.False(t, ok, "a new window starts from zero")

	// Keys the runtime does not own survive the rewrite.
	committed, ok := env.Get("kernel_override_abi_committed_A")
	assert.True(t, ok)
	assert.Equal(t, "", committed)
}

func TestArm_UnmountedBootPartitionIsNotMounted(t *testing.T) {
	seedBlock(t, "created.bootenv")
	prev := isMounted
	isMounted = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { isMounted = prev })

	assert.ErrorIs(t, Arm(strings.Repeat("3", 64)), ErrNotMounted)
}

func TestArm_AbsentBlockErrors(t *testing.T) {
	seedBlock(t, "created.bootenv")
	require.NoError(t, os.Remove(Path()))

	assert.Error(t, Arm(strings.Repeat("4", 64)))
}

// Two writers serialise on the file lock, so neither loses the other's key.
func TestUpdate_ConcurrentWritersKeepBothKeys(t *testing.T) {
	seedBlock(t, "created.bootenv")

	var wg sync.WaitGroup
	for _, key := range []string{"first", "second"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			assert.NoError(t, Update(func(env *Env) error { return env.Set(k, k+"-value") }))
		}(key)
	}
	wg.Wait()

	block, err := os.ReadFile(Path())
	require.NoError(t, err)
	env, err := Parse(block)
	require.NoError(t, err)

	for _, k := range []string{"first", "second"} {
		v, ok := env.Get(k)
		assert.True(t, ok, "%s must survive the other writer", k)
		assert.Equal(t, k+"-value", v)
	}
}

// An error from the callback leaves the block as it was.
func TestUpdate_CallbackFailureWritesNothing(t *testing.T) {
	path := seedBlock(t, "armed.bootenv")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Error(t, Update(func(env *Env) error { return errors.New("no") }))

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
