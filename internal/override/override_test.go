package override

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostTree redirects every host path at a temporary tree and returns its root.
func hostTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	prevState, prevBoot, prevVPN := StateMount, BootByABIDir, VPNActiveMarker
	prevEngine := DataEngineRoot
	StateMount = filepath.Join(root, "mnt", "state")
	BootByABIDir = filepath.Join(root, "mnt", "data", "boot-by-abi")
	DataEngineRoot = filepath.Join(root, "mnt", "data", "docker")
	VPNActiveMarker = filepath.Join(root, "run", "openvpn", "active")
	require.NoError(t, os.MkdirAll(StateMount, 0o755))
	require.NoError(t, os.MkdirAll(BootByABIDir, 0o755))

	t.Cleanup(func() {
		StateMount, BootByABIDir, VPNActiveMarker = prevState, prevBoot, prevVPN
		DataEngineRoot = prevEngine
	})
	return root
}

// TestKernelTarget pins the string the initramfs resolves. The engine's root
// and BootByABIDir share a parent, so the target reaches the kernel through
// it. A target built from the mountpoint the engine reports would cross the
// bind at /var/lib/docker and resolve only in the running OS.
func TestKernelTarget(t *testing.T) {
	hostTree(t)

	target, err := KernelTarget("volumes/ext_svc_abc_boot/_data", "Image")
	require.NoError(t, err)
	assert.Equal(t, "../docker/volumes/ext_svc_abc_boot/_data/Image", target)

	// A separator would reach outside the volume.
	for _, bad := range []string{"", ".", "..", "sub/Image", "../Image"} {
		_, err := KernelTarget("volumes/ext_svc_abc_boot/_data", bad)
		assert.Error(t, err, "%q is not a bare kernel image name", bad)
	}
}

// TestKernelTarget_FollowsTheHostPaths is why this lives beside BootByABIDir.
// Moving the published link moves what its target has to climb, and nothing
// outside this package has to learn about it.
func TestKernelTarget_FollowsTheHostPaths(t *testing.T) {
	root := hostTree(t)
	BootByABIDir = filepath.Join(root, "mnt", "data", "boot", "by-abi")

	target, err := KernelTarget("volumes/ext_svc_abc_boot/_data", "Image")
	require.NoError(t, err)
	assert.Equal(t, "../../docker/volumes/ext_svc_abc_boot/_data/Image", target)
}

// The pairing this package exists for: what validation records is what
// activation refuses on.
func TestRejection_RoundTrips(t *testing.T) {
	hostTree(t)

	listed, err := RejectedABI("aaaa")
	require.NoError(t, err)
	assert.False(t, listed, "an absent record is empty")

	require.NoError(t, RecordRejection("aaaa"))
	require.NoError(t, RecordRejection("bbbb"))
	// Appending an ABI already recorded leaves one line.
	require.NoError(t, RecordRejection("aaaa"))

	for _, abi := range []string{"aaaa", "bbbb"} {
		listed, err := RejectedABI(abi)
		require.NoError(t, err)
		assert.True(t, listed, "%s must be refused", abi)
	}
	listed, err = RejectedABI("cccc")
	require.NoError(t, err)
	assert.False(t, listed)

	record, err := os.ReadFile(RejectedPath())
	require.NoError(t, err)
	assert.Equal(t, "aaaa\nbbbb\n", string(record))
}

func TestRejection_UnreadableRecordIsAMachineCondition(t *testing.T) {
	hostTree(t)
	require.NoError(t, os.Mkdir(RejectedPath(), 0o755))

	_, err := RejectedABI("aaaa")
	assert.Error(t, err)
}

func TestRecordRejection_RefusesAnEmptyABI(t *testing.T) {
	hostTree(t)
	assert.Error(t, RecordRejection(""))
}

func TestAuditLine(t *testing.T) {
	tests := []struct {
		name string
		line Line
		want string
	}{
		{
			name: "a health rejection",
			line: Line{By: "health", From: "aaaa", To: "bbbb", Slot: "A"},
			want: "by=health from=aaaa to=bbbb slot=A",
		},
		{
			name: "a slot with no proven override",
			line: Line{By: "health", From: "aaaa", Slot: "B"},
			want: "by=health from=aaaa to=none slot=B",
		},
		{
			name: "a spent boot budget accounts for its attempts",
			line: Line{By: "boots", From: "aaaa", To: "bbbb", Slot: "A", Boots: "3"},
			want: "by=boots from=aaaa to=bbbb slot=A boots=3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hostTree(t)
			require.NoError(t, WriteAuditLine(tc.line))

			data, err := os.ReadFile(AuditPath())
			require.NoError(t, err)
			written := strings.TrimSuffix(string(data), "\n")

			stamp, rest, ok := strings.Cut(written, ": ")
			require.True(t, ok, "the line opens with a timestamp")
			_, err = time.Parse(time.RFC3339, stamp)
			assert.NoError(t, err, "Go spells the UTC offset Z; nothing parses this file")
			assert.Equal(t, tc.want, rest)
		})
	}
}

// The file carries the last rejection and no history.
func TestWriteAuditLine_OverwritesRatherThanAppends(t *testing.T) {
	hostTree(t)
	require.NoError(t, WriteAuditLine(Line{By: "boots", From: "aaaa", Slot: "A", Boots: "3"}))
	require.NoError(t, WriteAuditLine(Line{By: "health", From: "bbbb", Slot: "A"}))

	data, err := os.ReadFile(AuditPath())
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), "\n"))
	assert.Contains(t, string(data), "from=bbbb")
}

func TestPublishedKernels(t *testing.T) {
	hostTree(t)

	published, err := ListPublished()
	require.NoError(t, err)
	assert.Empty(t, published)

	require.NoError(t, PublishKernel("aaaa", "../docker/volumes/v/_data/Image"))
	// A republish overwrites, because a retry is a recreate.
	require.NoError(t, PublishKernel("aaaa", "../docker/volumes/w/_data/Image"))
	require.NoError(t, PublishKernel("bbbb", "../docker/volumes/x/_data/Image"))

	target, err := os.Readlink(filepath.Join(BootByABIDir, "aaaa"))
	require.NoError(t, err)
	assert.Equal(t, "../docker/volumes/w/_data/Image", target)

	// A dangling link is what the sweep collects.
	published, err = ListPublished()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"aaaa", "bbbb"}, published)

	found, err := KernelPublished("aaaa")
	require.NoError(t, err)
	assert.True(t, found)

	require.NoError(t, RemoveKernel("aaaa"))
	found, err = KernelPublished("aaaa")
	require.NoError(t, err)
	assert.False(t, found)

	// An absent link is the wanted state.
	require.NoError(t, RemoveKernel("aaaa"))
}

func TestListPublished_AbsentDirectoryIsEmpty(t *testing.T) {
	hostTree(t)
	require.NoError(t, os.RemoveAll(BootByABIDir))

	published, err := ListPublished()
	require.NoError(t, err)
	assert.Empty(t, published)
}

// A value read back from the block names a file, so a separator in one must
// not escape the directory.
func TestKernelLink_RefusesAnythingButABareName(t *testing.T) {
	hostTree(t)
	for _, abi := range []string{"", ".", "..", "a/b", "../aaaa", "/aaaa"} {
		_, err := KernelLink(abi)
		assert.Error(t, err, "%q must not name a link", abi)
	}
}

func TestHealthPrestate(t *testing.T) {
	hostTree(t)

	require.NoError(t, WriteHealthPrestate())
	value, err := os.ReadFile(HealthPrestatePath())
	require.NoError(t, err)
	assert.Equal(t, "BALENAOS_ROLLBACK_VPNONLINE=0\n", string(value))

	require.NoError(t, os.MkdirAll(filepath.Dir(VPNActiveMarker), 0o755))
	require.NoError(t, os.WriteFile(VPNActiveMarker, nil, 0o644))
	require.NoError(t, WriteHealthPrestate())
	value, err = os.ReadFile(HealthPrestatePath())
	require.NoError(t, err)
	assert.Equal(t, "BALENAOS_ROLLBACK_VPNONLINE=1\n", string(value))

	// A surviving temporary name would be adopted.
	entries, err := os.ReadDir(StateMount)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "extension-health-variables", entries[0].Name())

	require.NoError(t, RemoveHealthPrestate())
	_, err = os.Stat(HealthPrestatePath())
	assert.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, RemoveHealthPrestate())
}
