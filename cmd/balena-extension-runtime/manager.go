package main

import (
	"context"
	"fmt"

	"github.com/balena-os/balena-extension-runtime/internal/manager"
	"github.com/balena-os/balena-extension-runtime/internal/version"
	"github.com/spf13/cobra"
)

var managerRootCmd = &cobra.Command{
	Use:          "balena-extension-manager",
	Short:        "Manage hostapp extension lifecycle",
	Version:      fmt.Sprintf("%s (commit: %s)", version.Version, version.GitCommit),
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return initLogger()
	},
}

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove dead extension containers and, with --stale-os, os-stale containers and images",
	Long: "Remove dead extension containers. " +
		"Pass --stale-os to additionally remove containers whose " +
		"kernel-version or kernel-abi-id labels mismatch the running kernel, " +
		"and extension images whose io.balena.image.os-version label doesn't " +
		"match /etc/os-release VERSION_ID. " +
		"Stale-OS pruning is safe only after the HUP rollback-health commit.",
	RunE: func(cmd *cobra.Command, args []string) error {
		staleOS, _ := cmd.Flags().GetBool("stale-os")
		return manager.Cleanup(cmd.Context(), logger, manager.CleanupOpts{
			PruneStaleOS: staleOS,
		})
	},
}

func init() {
	managerRootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info",
		"Set the logging level (debug, info, warn, error)")
	cleanupCmd.Flags().Bool("stale-os", false,
		"Post-commit cleanup: also remove containers whose kernel-version or "+
			"kernel-abi-id labels mismatch the running kernel, and extension "+
			"images whose io.balena.image.os-version label doesn't match "+
			"/etc/os-release VERSION_ID.")
	managerRootCmd.AddCommand(cleanupCmd)
}

func ExecuteManager(ctx context.Context) error {
	return managerRootCmd.ExecuteContext(ctx)
}
