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

	// An empty value is set, not absent.
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

	// One byte short of the block.
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

	t.Cleanup(SetBootMount(dir))
	return filepath.Join(dir, "bootenv")
}

// seedBlockIn writes a block into an existing candidate mountpoint.
func seedBlockIn(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "bootenv")
	require.NoError(t, os.WriteFile(path, readFixture(t, "created.bootenv"), 0o644))
	return path
}

// One signed image is installed both ways: the boot partition is split only
// under secure boot. Which mounts come up is a runtime fact, so the block
// decides, not the mount table.
func TestResolveBlockPath_PicksTheCandidateHoldingTheBlock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		onNonenc bool
	}{
		{"a split device keeps the block on the non-encrypted mount", true},
		{"the block is read where it is, not where it is expected", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nonenc, boot := t.TempDir(), t.TempDir()
			holder := boot
			if tc.onNonenc {
				holder = nonenc
			}
			want := seedBlockIn(t, holder)
			defer setMounts(nonenc, boot, nonenc, boot)()

			got, err := resolveBlockPath()
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

// A signed image installed without secure boot never mounts the
// non-encrypted candidate. Without balena-bootloader it has no block.
func TestResolveBlockPath_ReportsNoBlockWhenAMountedCandidateHasNone(t *testing.T) {
	nonenc, boot := t.TempDir(), t.TempDir()
	defer setMounts(nonenc, boot, boot)()

	_, err := resolveBlockPath()
	assert.ErrorIs(t, err, ErrNoBlock)
	assert.NotErrorIs(t, err, ErrNotMounted)
}

// An unsecured install of a signed image never mounts the non-encrypted
// candidate and keeps the block on the other one. A found block outranks an
// absent candidate, or every such device reports ErrNotMounted for good.
func TestResolveBlockPath_PrefersAFoundBlockOverAnAbsentCandidate(t *testing.T) {
	nonenc, boot := t.TempDir(), t.TempDir()
	want := seedBlockIn(t, boot)
	defer setMounts(nonenc, boot, boot)()

	got, err := resolveBlockPath()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// Every candidate mounted and none holding a block is a device with no
// override axis, which is not a defect.
func TestResolveBlockPath_ReportsNoBlockWhenEveryCandidateIsMounted(t *testing.T) {
	nonenc, boot := t.TempDir(), t.TempDir()
	defer setMounts(nonenc, boot, nonenc, boot)()

	_, err := resolveBlockPath()
	assert.ErrorIs(t, err, ErrNoBlock)
	// Every candidate was stat'd, so the error names every block path.
	assert.Contains(t, err.Error(), filepath.Join(nonenc, "bootenv"))
	assert.Contains(t, err.Error(), filepath.Join(boot, "bootenv"))
}

// Two blocks mean the OS resolved the mount differently from this binary.
// A verdict written to the file nobody reads is worse than a refusal.
func TestResolveBlockPath_RefusesWhenBothCandidatesHoldABlock(t *testing.T) {
	nonenc, boot := t.TempDir(), t.TempDir()
	first, second := seedBlockIn(t, nonenc), seedBlockIn(t, boot)
	defer setMounts(nonenc, boot, nonenc, boot)()

	_, err := resolveBlockPath()
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoBlock)
	assert.NotErrorIs(t, err, ErrNotMounted)
	assert.Contains(t, err.Error(), first)
	assert.Contains(t, err.Error(), second)
}

// The operator must learn every path that was tried, not only the first.
func TestResolveBlockPath_NamesEveryCandidateWhenNoneIsMounted(t *testing.T) {
	nonenc, boot := t.TempDir(), t.TempDir()
	defer setMounts(nonenc, boot)()

	_, err := resolveBlockPath()
	require.ErrorIs(t, err, ErrNotMounted)
	assert.Contains(t, err.Error(), nonenc)
	assert.Contains(t, err.Error(), boot)
}

// An unsplit device links the same path into both variables. Counted twice,
// its one block would read as the two the resolver refuses.
func TestResolveBlockPath_UnsplitMountIsOneCandidate(t *testing.T) {
	dir := t.TempDir()
	want := seedBlockIn(t, dir)
	defer setMounts(dir, dir, dir)()

	got, err := resolveBlockPath()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// mounts.IsMounted compares cleaned paths, so one mountpoint spelled two ways
// is one mount. Counting it twice would refuse a device that has one block.
func TestResolveBlockPath_UnsplitMountSpelledTwoWaysIsOneCandidate(t *testing.T) {
	dir := t.TempDir()
	want := seedBlockIn(t, dir)
	defer setMounts(dir+"/", dir, dir)()

	got, err := resolveBlockPath()
	require.NoError(t, err)
	assert.Equal(t, want, got)
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

// The arm reaches a path and the kernel command line in stage 2.
func TestArm_RefusesAMalformedABIAndWritesNothing(t *testing.T) {
	path := seedBlock(t, "armed.bootenv")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	valid := strings.Repeat("a", 64)
	for _, abi := range []string{
		"",
		valid[:63],
		valid + "0",
		strings.ToUpper(valid),
		valid[:60] + " x=y",
		"../" + valid[:61],
	} {
		assert.Error(t, Arm(abi), "%q", abi)
	}

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestArm_UnmountedBootPartitionIsNotMounted(t *testing.T) {
	seedBlock(t, "created.bootenv")
	prev := isMounted
	isMounted = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { isMounted = prev })

	assert.ErrorIs(t, Arm(strings.Repeat("3", 64)), ErrNotMounted)
}

func TestArm_AbsentBlockErrors(t *testing.T) {
	require.NoError(t, os.Remove(seedBlock(t, "created.bootenv")))

	assert.Error(t, Arm(strings.Repeat("4", 64)))
}

// Two writers serialise on the file lock, so neither loses the other's key.
func TestUpdate_ConcurrentWritersKeepBothKeys(t *testing.T) {
	path := seedBlock(t, "created.bootenv")

	var wg sync.WaitGroup
	for _, key := range []string{"first", "second"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			assert.NoError(t, Update(func(env *Env) error { return env.Set(k, k+"-value") }))
		}(key)
	}
	wg.Wait()

	block, err := os.ReadFile(path)
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
