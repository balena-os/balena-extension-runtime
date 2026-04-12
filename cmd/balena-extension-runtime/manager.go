package main

import (
	"context"
	"fmt"

	"github.com/balena-os/balena-extension-runtime/internal/manager"
	"github.com/balena-os/balena-extension-runtime/internal/version"
	"github.com/spf13/cobra"
)

var managerRootCmd = &cobra.Command{
	Use:     "balena-extension-manager",
	Short:   "Manage hostapp extension lifecycle",
	Version: fmt.Sprintf("%s (commit: %s)", version.Version, version.GitCommit),
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return initLogger()
	},
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Re-create extension containers for the new kernel version",
	RunE: func(cmd *cobra.Command, args []string) error {
		rootfs, _ := cmd.Flags().GetString("rootfs")
		return manager.Update(context.Background(), logger, rootfs)
	},
}

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove stale extension containers and orphaned images",
	RunE: func(cmd *cobra.Command, args []string) error {
		return manager.Cleanup(context.Background(), logger)
	},
}

func init() {
	managerRootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info",
		"Set the logging level (debug, info, warn, error)")
	updateCmd.Flags().String("rootfs", "", "Path to the new OS rootfs")
	if err := updateCmd.MarkFlagRequired("rootfs"); err != nil {
		panic(err) // flag was just registered above
	}
	managerRootCmd.AddCommand(updateCmd, cleanupCmd)
}

func ExecuteManager() error {
	return managerRootCmd.Execute()
}
