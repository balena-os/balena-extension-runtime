package oci

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"sync"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// NormalizeBundlePath returns a cleaned absolute form of bundlePath. It
// rejects empty input. The result is safe to pass to ReadSpec and
// ResolveRootfs — both rely on bundlePath being absolute so that a relative
// spec.Root.Path cannot be reinterpreted against the caller's working
// directory.
func NormalizeBundlePath(bundlePath string) (string, error) {
	if bundlePath == "" {
		return "", fmt.Errorf("bundle path must not be empty")
	}
	abs, err := filepath.Abs(bundlePath)
	if err != nil {
		return "", fmt.Errorf("resolve bundle path %q: %w", bundlePath, err)
	}
	return filepath.Clean(abs), nil
}

// ReadSpec reads and parses the OCI config.json from the given bundle path.
func ReadSpec(bundlePath string) (*specs.Spec, error) {
	specPath := filepath.Join(bundlePath, "config.json")
	f, err := os.Open(specPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open config.json: %w", err)
	}
	defer func() { _ = f.Close() }()

	var s specs.Spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, fmt.Errorf("failed to decode config.json: %w", err)
	}
	return &s, nil
}

// ResolveRootfs returns the absolute path to the rootfs declared by the spec.
// When spec.Root.Path is relative, it is joined with bundlePath and the
// result is required to stay within bundlePath — a "rootfs": "../../etc"
// spec is rejected. Absolute rootfs paths (common with overlay2 backends
// that point at /var/lib/docker/...) are accepted as-is.
func ResolveRootfs(spec *specs.Spec, bundlePath string) (string, error) {
	if spec == nil || spec.Root == nil {
		return "", fmt.Errorf("spec.root is missing")
	}
	rootfs := spec.Root.Path
	if rootfs == "" {
		return "", fmt.Errorf("spec.root.path is empty")
	}
	if filepath.IsAbs(rootfs) {
		return filepath.Clean(rootfs), nil
	}
	bundle := filepath.Clean(bundlePath)
	joined := filepath.Clean(filepath.Join(bundle, rootfs))
	// Guard against spec.Root.Path traversal (e.g. "../etc"). Allow the
	// bundle itself (rare: Root.Path == ".") but not anything outside.
	if joined != bundle && !strings.HasPrefix(joined, bundle+string(filepath.Separator)) {
		return "", fmt.Errorf("rootfs %q escapes bundle %q", spec.Root.Path, bundle)
	}
	return joined, nil
}

var (
	dockerRootMu sync.RWMutex
	dockerRoot   = "/var/lib/docker"
)

// SetDockerRoot sets the Docker data root directory used by EnrichAnnotations
// to locate container metadata. It should be called with the value of the
// --docker-root flag before any runtime operations. Safe for concurrent use.
func SetDockerRoot(root string) {
	dockerRootMu.Lock()
	defer dockerRootMu.Unlock()
	dockerRoot = root
}

func getDockerRoot() string {
	dockerRootMu.RLock()
	defer dockerRootMu.RUnlock()
	return dockerRoot
}

// dockerConfig is the subset of config.v2.json we need.
type dockerConfig struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// EnrichAnnotations copies Docker container labels into spec.Annotations when
// the annotations are absent. balena-engine does not propagate container labels
// to the OCI spec annotations field, so the runtime reads them directly from
// the Docker container store as a fallback.
//
// The default Docker root is /var/lib/docker; call SetDockerRoot to override.
// containerID is the OCI container ID (same as the Docker container ID).
//
// This is best-effort: if the Docker config is missing or unparseable, the
// reason is logged at debug and validation will later fail with "missing
// required label". The debug log is what lets you diagnose that situation.
func EnrichAnnotations(logger *slog.Logger, spec *specs.Spec, containerID string) {
	if len(spec.Annotations) > 0 {
		return // already populated (e.g. synthetic test bundles)
	}
	// Validate before touching the filesystem — a crafted ID like
	// "../../../etc" would otherwise be joined into configPath and os.Open'd
	// against dockerRoot.
	if err := ValidateContainerID(containerID); err != nil {
		logger.Debug("skipping annotation enrichment: invalid container ID",
			"id", containerID, "err", err)
		return
	}
	configPath := filepath.Join(getDockerRoot(), "containers", containerID, "config.v2.json")

	f, err := os.Open(configPath)
	if err != nil {
		logger.Debug("could not read docker container config for label fallback",
			"path", configPath, "err", err)
		return
	}
	defer func() { _ = f.Close() }()

	var dc dockerConfig
	if err := json.NewDecoder(f).Decode(&dc); err != nil {
		logger.Debug("could not decode docker container config",
			"path", configPath, "err", err)
		return
	}
	if len(dc.Config.Labels) == 0 {
		logger.Debug("docker container config has no labels", "path", configPath)
		return
	}
	spec.Annotations = make(map[string]string, len(dc.Config.Labels))
	for k, v := range dc.Config.Labels {
		spec.Annotations[k] = v
	}
}
