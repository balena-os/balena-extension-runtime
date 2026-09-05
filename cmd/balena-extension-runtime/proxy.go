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

		// Block until signaled. The proxy is the container's process, so its
		// exit status is what the engine records as the container's verdict:
		// SIGUSR1 is `start` reporting the activation succeeded (exit 0),
		// SIGUSR2 is `start` reporting the extension refused it (exit 1),
		// SIGTERM/SIGINT are normal stops (exit 0). Returning nil lets
		// cobra/main exit with code 0.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGTERM, syscall.SIGINT)
		if sig := <-sigCh; sig == syscall.SIGUSR2 {
			// Exit directly rather than returning an error: the non-zero
			// status is the message, and routing it through main's error
			// path would log a runtime failure that did not happen.
			CloseLogger()
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	proxyCmd.Flags().StringVar(&proxyContainerID, "id", "", "Container ID")
	rootCmd.AddCommand(proxyCmd)
}
