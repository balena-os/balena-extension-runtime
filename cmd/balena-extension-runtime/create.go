package main

import (
	"github.com/balena-os/balena-extension-runtime/internal/runtime"
	"github.com/spf13/cobra"
)

var (
	bundlePath string
	pidFile    string
)

var createCmd = &cobra.Command{
	Use:   "create <container-id>",
	Short: "Create a new extension container from an OCI bundle",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		containerID := args[0]
		if bundlePath == "" {
			bundlePath = "."
		}
		return runtime.Create(cmd.Context(), logger, containerID, bundlePath, pidFile)
	},
}

func init() {
	createCmd.Flags().StringVar(&bundlePath, "bundle", "",
		"Path to the OCI bundle directory (defaults to current directory)")
	createCmd.Flags().StringVar(&pidFile, "pid-file", "",
		"File to write the proxy process PID to")
	rootCmd.AddCommand(createCmd)
}
