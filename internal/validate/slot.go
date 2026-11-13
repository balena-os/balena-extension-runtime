package validate

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
)

// The host facts the slot derivation reads, as variables so tests can
// redirect them.
var (
	procMounts = "/proc/mounts"
	activeRoot = "/mnt/sysroot/active"
	byLabelDir = "/dev/disk/by-label"
)

// The whole candidate set. The prefix is per-release.
var rootLabels = map[string]bootenv.Slot{
	"resin-rootA":  bootenv.SlotA,
	"resin-rootB":  bootenv.SlotB,
	"balena-rootA": bootenv.SlotA,
	"balena-rootB": bootenv.SlotB,
}

// RunningSlot names the root filesystem this boot is running from.
//
// A source no candidate label resolves to is a defect, not a guess: writing
// the wrong slot's committed value is how a proven kernel gets retired.
func RunningSlot() (bootenv.Slot, error) {
	source, err := activeSource()
	if err != nil {
		return "", err
	}
	for label, slot := range rootLabels {
		if sameDevice(filepath.Join(byLabelDir, label), source) {
			return slot, nil
		}
	}
	return "", fmt.Errorf("no root filesystem label under %s names %s, the source of %s",
		byLabelDir, source, activeRoot)
}

// activeSource reads the device backing the running root filesystem.
//
// The mount can appear twice, bind-mounted over itself while old hooks run
// from the inactive partition; both entries name the same source.
func activeSource() (string, error) {
	f, err := os.Open(procMounts)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", procMounts, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[1] == activeRoot {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", procMounts, err)
	}
	return "", fmt.Errorf("%s is not in %s", activeRoot, procMounts)
}

// sameDevice reports whether labelPath and source name one block device.
// Stat follows a symlink, so a by-label link and the node it names compare
// equal, and so do two nodes with one device number.
//
// udev's by-label link names the node that holds the filesystem: the
// partition on a plain device, the kernel dm node on LUKS. A plain mount
// source is that same node. A LUKS source as /dev/mapper/luks-<uuid> can be
// a second device node as /dev/dm-N, so the paths differ while the device
// numbers match.
func sameDevice(labelPath, source string) bool {
	label, labelOK := deviceNumber(labelPath)
	src, srcOK := deviceNumber(source)
	return labelOK && srcOK && label == src
}

// deviceNumber returns the device number of a block node. Stat follows a
// symlink to the node it names.
func deviceNumber(path string) (uint64, bool) {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return 0, false
	}
	return uint64(st.Rdev), true
}
