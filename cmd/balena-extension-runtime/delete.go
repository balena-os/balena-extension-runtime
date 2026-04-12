package main

import (
	"github.com/balena-os/balena-extension-runtime/internal/runtime"
	"github.com/spf13/cobra"
)

var forceDelete bool

var deleteCmd = &cobra.Command{
	Use:   "delete <container-id>",
	Short: "Delete an extension container",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runtime.Delete(logger, args[0], forceDelete)
	},
}

func init() {
	deleteCmd.Flags().BoolVarP(&forceDelete, "force", "f", false,
		"Force delete a running container")
	rootCmd.AddCommand(deleteCmd)
}
