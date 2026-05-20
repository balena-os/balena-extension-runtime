package mounts

import (
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
// "/" and "-" with "_", then uppercased. Callers should ensure destinations
// do not differ only in those characters, as they would normalize to the
// same key; the last one in sorted order wins.
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
