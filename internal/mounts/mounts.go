package mounts

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
)

const envPrefix = "EXTENSION_VOLUME_"

// ToEnv produces EXTENSION_VOLUME_<DEST>=<source> entries for each mount with
// an absolute destination, sorted by destination for deterministic ordering.
// Non-absolute, empty, or root ("/") destinations are skipped.
//
// Destinations are normalized by stripping the leading "/" and replacing
// "/" and "-" with "_", then uppercased. Two destinations that differ only
// in those characters (e.g. "/var-lib" and "/var/lib") normalize to the same
// key; this function does not dedup, so it emits both entries and the
// consuming process picks one per its own getenv semantics (glibc returns the
// first). Callers should avoid declaring such colliding destinations.
func ToEnv(mounts []specs.Mount) []string {
	if len(mounts) == 0 {
		return nil
	}
	sorted := make([]specs.Mount, 0, len(mounts))
	for _, m := range mounts {
		if !strings.HasPrefix(m.Destination, "/") {
			continue
		}
		sorted = append(sorted, m)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Destination < sorted[j].Destination
	})

	out := make([]string, 0, len(sorted))
	for _, m := range sorted {
		dest := strings.TrimPrefix(m.Destination, "/")
		dest = strings.ReplaceAll(dest, "/", "_")
		dest = strings.ReplaceAll(dest, "-", "_")
		dest = strings.ToUpper(dest)
		if dest == "" {
			continue
		}
		out = append(out, envPrefix+dest+"="+m.Source)
	}
	return out
}

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
