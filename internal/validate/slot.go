package validate

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
)

// The host facts the slot derivation reads, as variables so tests can
// redirect them.
var (
	procMounts = "/proc/mounts"
	activeRoot = "/mnt/sysroot/active"
	byLabelDir = "/dev/disk/by-label"
)

// The whole candidate set. The prefix is per-release and udev publishes the
// inner filesystem's label, so on a LUKS device the dm source resolves here
// without a special case.
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
	resolved := resolve(source)

	for label, slot := range rootLabels {
		if resolve(filepath.Join(byLabelDir, label)) == resolved {
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

// resolve follows a device link, leaving a source that is not a path alone
// so it can still fail the comparison rather than the derivation.
func resolve(path string) string {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		return target
	}
	return path
}
