package main

import (
	"encoding/json"
	"fmt"

	"github.com/balena-os/balena-extension-runtime/internal/version"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-spec/specs-go/features"
	"github.com/spf13/cobra"
)

// disabled reports a subsystem as unsupported. A nil field is dropped by
// omitempty and reads as "unknown", not as "false".
func disabled() *bool {
	v := false
	return &v
}

// runtimeFeatures describes what this runtime really honours. It reads only
// the root path and the annotations of a bundle spec. hooks and mountOptions
// stay out, because the schema cannot say "none" for a list.
func runtimeFeatures() features.Features {
	return features.Features{
		OCIVersionMin: "1.0.0",
		OCIVersionMax: specs.Version,
		Linux: &features.Linux{
			Cgroup: &features.Cgroup{
				V1:          disabled(),
				V2:          disabled(),
				Systemd:     disabled(),
				SystemdUser: disabled(),
				Rdma:        disabled(),
			},
			Seccomp:  &features.Seccomp{Enabled: disabled()},
			Apparmor: &features.Apparmor{Enabled: disabled()},
			Selinux:  &features.Selinux{Enabled: disabled()},
			IntelRdt: &features.IntelRdt{Enabled: disabled()},
			MountExtensions: &features.MountExtensions{
				IDMap: &features.IDMap{Enabled: disabled()},
			},
		},
		Annotations: map[string]string{
			"io.balena.extension-runtime.version": version.Version,
			"io.balena.extension-runtime.commit":  version.GitCommit,
		},
	}
}

var featuresCmd = &cobra.Command{
	Use:   "features",
	Short: "Print the OCI runtime features this runtime supports",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		output, err := json.Marshal(runtimeFeatures())
		if err != nil {
			return fmt.Errorf("failed to marshal features: %w", err)
		}

		fmt.Println(string(output))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(featuresCmd)
}
