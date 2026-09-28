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

// SetDockerRoot sets the Docker data root. ReadIdentity locates container
// metadata under it, and VolumeDataDir locates volume data. Call it with the
// --docker-root flag value before any runtime operation. Safe for concurrent
// use.
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

// dockerConfig is the subset of config.v2.json we need. Image is the id of
// the image the container was created from, in "sha256:<hex>" form.
type dockerConfig struct {
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Identity is what create resolves once about a container's extension, and
// what start reads back from the OCI state.
type Identity struct {
	// Labels are the container's labels as the engine recorded them. The
	// manager's volume sweep is handed the same map, so volume identity comes
	// from these and not from spec.Annotations: a create that named the volume
	// from the other map would strand it. With no store to read, the spec's
	// annotations are the whole identity.
	Labels map[string]string

	// ImageID is the digest of the image the container was created from, in
	// "sha256:<hex>" form, or "" when there was no store to read.
	ImageID string
}

// ReadIdentity resolves which extension a container holds. balena-engine does
// not copy container labels into OCI spec annotations, so the container store
// comes first. When config.v2.json reads, both fields come from it. Otherwise
// the spec's annotations are the identity, and there is no image id.
//
// One source gives both fields. A store container with no labels gets no
// fallback to the spec, so class validation reports the missing label.
// ReadIdentity does not modify spec.
//
// The default Docker root is /var/lib/docker. Call SetDockerRoot to override.
func ReadIdentity(logger *slog.Logger, spec *specs.Spec, containerID string) Identity {
	if dc, ok := readDockerConfig(logger, containerID); ok {
		return Identity{Labels: dc.Config.Labels, ImageID: dc.Image}
	}
	return Identity{Labels: spec.Annotations}
}

// VolumeRelDir returns a volume's data directory, relative to the Docker data
// root. It holds the only copy of the engine's volume layout, and the only
// check of a volume name.
func VolumeRelDir(name string) (string, error) {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("volume name %q is not a bare file name", name)
	}
	return filepath.Join("volumes", name, "_data"), nil
}

// VolumeDataDir returns where the engine holds a volume's data under the
// configured Docker data root.
func VolumeDataDir(name string) (string, error) {
	rel, err := VolumeRelDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(getDockerRoot(), rel), nil
}

// readDockerConfig loads the container store's config.v2.json for containerID.
// The bool reports whether the file was read and decoded; every failure is a
// debug log rather than an error, because the caller has a usable fallback
// for each of them.
func readDockerConfig(logger *slog.Logger, containerID string) (dockerConfig, bool) {
	var dc dockerConfig
	// Validate before touching the filesystem — a crafted ID like
	// "../../../etc" would otherwise be joined into configPath and os.Open'd
	// against dockerRoot.
	if err := ValidateContainerID(containerID); err != nil {
		logger.Debug("skipping container store lookup: invalid container ID",
			"id", containerID, "err", err)
		return dc, false
	}
	configPath := filepath.Join(getDockerRoot(), "containers", containerID, "config.v2.json")

	f, err := os.Open(configPath)
	if err != nil {
		logger.Debug("could not read docker container config",
			"path", configPath, "err", err)
		return dc, false
	}
	defer func() { _ = f.Close() }()

	if err := json.NewDecoder(f).Decode(&dc); err != nil {
		logger.Debug("could not decode docker container config",
			"path", configPath, "err", err)
		return dc, false
	}
	if len(dc.Config.Labels) == 0 {
		logger.Debug("docker container config has no labels", "path", configPath)
	}
	return dc, true
}
