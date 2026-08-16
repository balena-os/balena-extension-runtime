package main

import (
	"os"

	"github.com/balena-os/balena-extension-runtime/internal/proxy"
	"github.com/spf13/cobra"
)

var proxyContainerID string

var proxyCmd = &cobra.Command{
	Use:    "proxy",
	Short:  "Proxy process that provides a PID for the containerd shim",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		logger.Debug("proxy started", "container", proxyContainerID)

		if status := proxy.Run(); status != 0 {
			// The non-zero status is the message, not an error.
			CloseLogger()
			os.Exit(status)
		}
		return nil
	},
}

func init() {
	proxyCmd.Flags().StringVar(&proxyContainerID, "id", "", "Container ID")
	rootCmd.AddCommand(proxyCmd)
}
