package labels

import "fmt"

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

	// OSVersion is the HUP-commit retention predicate for extension images.
	// Value is a comma-separated list of shell-style globs; any match against
	// /etc/os-release VERSION_ID retains the image. Missing/empty = retain
	// (legacy-safe default).
	OSVersion = Prefix + "os-version"
)

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
