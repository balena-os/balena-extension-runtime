package labels

import (
	"fmt"
	"strings"
)

const (
	Prefix = "io.balena.image."

	// Class identifies the extension type. Required.
	Class = Prefix + "class"

	// ClassOverlay is the only supported class value.
	ClassOverlay = "overlay"

	// KernelVersion declares kernel ABI version (M.m.p) for userspace compatibility. Optional.
	KernelVersion = Prefix + "kernel-version"

	// KernelABIID identifies the kernel's binary interface for module/eBPF compatibility. Optional.
	KernelABIID = Prefix + "kernel-abi-id"

	// ServiceName is the compose service the container was deployed from,
	// used to name fabricated volumes.
	ServiceName = "io.balena.service-name"

	// OSVersion is the HUP-commit retention predicate for extension images.
	// Value is a comma-separated list of shell-style globs; any match against
	// /etc/os-release VERSION_ID retains the image. Missing/empty = retain
	// (legacy-safe default).
	OSVersion = Prefix + "os-version"
)

// shortIDLen is how much of a container id, and of an image digest, is worth
// carrying.
const shortIDLen = 12

// FabricatesVolume reports whether an extension gets a /boot volume fabricated
// for it based on the existence of kernel ABI id.
//
// Create and cleanup's volume sweep must agree on this, or the sweep derives
// no name for a fabricated volume and collects it while its container is still
// there.
func FabricatesVolume(lbls map[string]string) bool {
	return lbls[KernelABIID] != ""
}

// resolveServiceName returns the key a fabricated volume is named after. A
// manual deploy carries no service label, so the container id prefix stands in.
func resolveServiceName(lbls map[string]string, containerID string) string {
	if name := lbls[ServiceName]; name != "" {
		return name
	}
	return ShortID(containerID)
}

// BootVolume returns the name of the /boot volume a kernel override gets, or
// "" for an extension that gets none. create fabricates the volume, and
// cleanup's retention guard re-derives the name, so both go through here.
func BootVolume(lbls map[string]string, containerID, imageID string) (string, error) {
	if !FabricatesVolume(lbls) {
		return "", nil
	}
	if imageID == "" {
		return "", fmt.Errorf("no image id for container %s, so its volume cannot be named",
			ShortID(containerID))
	}
	return VolumeName(resolveServiceName(lbls, containerID), imageID), nil
}

// VolumeName derives the name of the volume backing /boot for an extension.
func VolumeName(service, imageID string) string {
	return fmt.Sprintf("ext_%s_%s_boot", service, ShortID(strings.TrimPrefix(imageID, "sha256:")))
}

// Image returns the io.balena.image.* subset of a label set.
func Image(lbls map[string]string) map[string]string {
	selected := make(map[string]string, len(lbls))
	for k, v := range lbls {
		if strings.HasPrefix(k, Prefix) {
			selected[k] = v
		}
	}
	return selected
}

// ShortID trims an id to the prefix the manager's volume names and log lines
// share. It does not assume the caller holds a full-length engine id.
func ShortID(id string) string {
	if len(id) > shortIDLen {
		return id[:shortIDLen]
	}
	return id
}

// Validate checks that the OCI annotations contain the required extension labels.
func Validate(annotations map[string]string) error {
	class, ok := annotations[Class]
	if !ok {
		return fmt.Errorf("missing required label %s", Class)
	}
	if class != ClassOverlay {
		return fmt.Errorf("unsupported %s=%q, must be %q", Class, class, ClassOverlay)
	}
	return nil
}
