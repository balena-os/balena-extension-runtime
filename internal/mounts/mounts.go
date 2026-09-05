package mounts

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A variable so tests can point it at a fixture.
var procMounts = "/proc/mounts"

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
