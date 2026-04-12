package main

import (
	"github.com/balena-os/balena-extension-runtime/internal/runtime"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start <container-id>",
	Short: "Start an extension container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		return runtime.Start(logger, args[0])
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
