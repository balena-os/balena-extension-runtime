package oci

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/opencontainers/runtime-spec/specs-go"
)

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

// ResolveRootfs returns the absolute path to the rootfs from the OCI spec.
func ResolveRootfs(spec *specs.Spec, bundlePath string) string {
	rootfs := spec.Root.Path
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(bundlePath, rootfs)
	}
	return rootfs
}

var dockerRoot = "/var/lib/docker"

// SetDockerRoot sets the Docker data root directory used by EnrichAnnotations
// to locate container metadata. It should be called with the value of the
// --docker-root flag before any runtime operations.
func SetDockerRoot(root string) {
	dockerRoot = root
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
	configPath := filepath.Join(dockerRoot, "containers", containerID, "config.v2.json")

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
