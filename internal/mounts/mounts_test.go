package mounts

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsMounted(t *testing.T) {
	table := filepath.Join(t.TempDir(), "mounts")
	require.NoError(t, os.WriteFile(table, []byte(
		"/dev/mmcblk0p1 /mnt/boot vfat rw,relatime 0 0\n"+
			"/dev/mmcblk0p5 /mnt/state ext4 rw,relatime 0 0\n"), 0o644))

	prev := procMounts
	procMounts = table
	t.Cleanup(func() { procMounts = prev })

	mounted, err := IsMounted("/mnt/state")
	require.NoError(t, err)
	assert.True(t, mounted)

	mounted, err = IsMounted("/mnt/data")
	require.NoError(t, err)
	assert.False(t, mounted, "a path that is not a mountpoint is not mounted")

	// A prefix of a mountpoint is not one.
	mounted, err = IsMounted("/mnt")
	require.NoError(t, err)
	assert.False(t, mounted)
}

// An unreadable mount table is a machine condition the caller has to be able
// to tell apart from "not mounted".
func TestIsMounted_UnreadableTableErrors(t *testing.T) {
	prev := procMounts
	procMounts = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { procMounts = prev })

	_, err := IsMounted("/mnt/state")
	assert.Error(t, err)
}

func TestMountpointOfDev(t *testing.T) {
	table := filepath.Join(t.TempDir(), "mountinfo")
	require.NoError(t, os.WriteFile(table, []byte(
		"21 29 179:1 /EFI /mnt/efi-bind rw,relatime - vfat /dev/mmcblk0p1 rw\n"+
			"22 29 179:1 / /mnt/boot rw,relatime - vfat /dev/mmcblk0p1 rw\n"+
			"23 29 253:0 / /mnt/with\\040space rw - ext4 /dev/mapper/boot rw\n"+
			"24 29 179:1 / /resin-boot rw,relatime - vfat /dev/mmcblk0p1 rw\n"), 0o644))
	prev := procMountinfo
	procMountinfo = table
	t.Cleanup(func() { procMountinfo = prev })

	got, err := mountpointOfDev(179, 1)
	require.NoError(t, err)
	assert.Equal(t, "/mnt/boot", got, "the first mount of the device root serves")

	got, err = mountpointOfDev(253, 0)
	require.NoError(t, err)
	assert.Equal(t, "/mnt/with space", got)

	got, err = mountpointOfDev(179, 2)
	require.NoError(t, err)
	assert.Empty(t, got, "an unmounted device is not an error")
}

func TestMountpointOf_RejectsWhatIsNotABlockDevice(t *testing.T) {
	_, err := MountpointOf("/dev/null")
	assert.Error(t, err, "a character device shares no major:minor space with block devices")

	_, err = MountpointOf(filepath.Join(t.TempDir(), "absent"))
	assert.Error(t, err)
}

// Values a device carries: mmcblk0p1 is 179:1, dm-0 is 253:0, and a minor
// past 255 spills into the high bits.
func TestDevSplit(t *testing.T) {
	for _, tc := range []struct{ major, minor uint32 }{{179, 1}, {253, 0}, {8, 257}} {
		dev := uint64(tc.major&0xfff)<<8 | uint64(tc.major&^0xfff)<<32 | uint64(tc.minor&0xff) | uint64(tc.minor&^0xff)<<12
		assert.Equal(t, tc.major, major(dev))
		assert.Equal(t, tc.minor, minor(dev))
	}
}
