package main

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/balena-os/balena-extension-runtime/internal/runtime"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <container-id> [SIGNAL]",
	Short: "Send a signal to the extension container process",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		containerID := args[0]

		signal := syscall.SIGTERM
		if len(args) > 1 {
			var err error
			signal, err = parseSignal(args[1])
			if err != nil {
				return err
			}
		}

		return runtime.Kill(logger, containerID, signal)
	},
}

func init() {
	rootCmd.AddCommand(killCmd)
}

func parseSignal(input string) (syscall.Signal, error) {
	switch strings.ToUpper(input) {
	case "KILL", "SIGKILL", "9":
		return syscall.SIGKILL, nil
	case "TERM", "SIGTERM", "15":
		return syscall.SIGTERM, nil
	case "INT", "SIGINT", "2":
		return syscall.SIGINT, nil
	case "USR1", "SIGUSR1", "10":
		return syscall.SIGUSR1, nil
	default:
		return 0, fmt.Errorf("unsupported signal: %s", input)
	}
}
