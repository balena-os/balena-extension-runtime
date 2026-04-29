package main

import (
	"os"
	"os/signal"
	"syscall"

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

		// Block until SIGUSR1 (start complete), SIGTERM, or SIGINT. All three
		// mean "exit cleanly" — SIGUSR1 is how `start` signals that the
		// extension has finished installing; SIGTERM/SIGINT are normal stops.
		// Returning nil lets cobra/main exit with code 0.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		return nil
	},
}

func init() {
	proxyCmd.Flags().StringVar(&proxyContainerID, "id", "", "Container ID")
	rootCmd.AddCommand(proxyCmd)
}
