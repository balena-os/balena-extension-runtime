package mounts

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Variables so tests can point them at fixtures.
var (
	procMounts    = "/proc/mounts"
	procMountinfo = "/proc/self/mountinfo"
)

// IsMounted reports whether path is a mountpoint. An unreadable table errors
// rather than returning false: callers will not write what they cannot place.
func IsMounted(path string) (bool, error) {
	f, err := os.Open(procMounts)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", procMounts, err)
	}
	defer func() { _ = f.Close() }()

	target := filepath.Clean(path)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		// Targets with spaces are escaped and never match.
		if fields[1] == target {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", procMounts, err)
	}
	return false, nil
}

// MountpointOf returns where the block device's root is mounted, or "" when
// it is not. Every mount of that root holds the same files, so the first
// entry is enough.
func MountpointOf(device string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(device, &st); err != nil {
		return "", fmt.Errorf("stat %s: %w", device, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return "", fmt.Errorf("%s is not a block device", device)
	}
	return mountpointOfDev(major(uint64(st.Rdev)), minor(uint64(st.Rdev)))
}

// mountpointOfDev finds the mount of the root of the device with this
// major:minor, which is how mountinfo names a filesystem's device, dm nodes
// included.
func mountpointOfDev(major, minor uint32) (string, error) {
	f, err := os.Open(procMountinfo)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", procMountinfo, err)
	}
	defer func() { _ = f.Close() }()

	want := fmt.Sprintf("%d:%d", major, minor)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		// A bind of a subdirectory does not hold the block.
		if fields[2] == want && fields[3] == "/" {
			return unescapeMount.Replace(fields[4]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %s: %w", procMountinfo, err)
	}
	return "", nil
}

// mountinfo escapes these as octal.
var unescapeMount = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// The Linux dev_t layout, as glibc's gnu_dev_major and gnu_dev_minor read it.
func major(dev uint64) uint32 { return uint32((dev>>8)&0xfff | (dev>>32)&^uint64(0xfff)) }
func minor(dev uint64) uint32 { return uint32(dev&0xff | (dev>>12)&^uint64(0xff)) }
