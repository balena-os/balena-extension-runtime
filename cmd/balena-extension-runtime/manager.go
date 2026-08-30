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
	Short: "Remove garbage extension containers and unclaimed volumes, and with --stale-os, stale containers and images",
	Long: "Remove extension containers that the engine reports dead or whose " +
		"runtime create failed. Then remove the fabricated /boot volumes that " +
		"no remaining container claims. With --stale-os, also remove extension " +
		"containers and images whose kernel-abi-id, kernel-version or " +
		"os-version label the running system does not satisfy. Use --stale-os " +
		"only after a host OS update commits. Before that, a stale image is " +
		"the rollback target.",
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
		"Also remove extension containers and images whose kernel-abi-id, "+
			"kernel-version or os-version label the running system does not "+
			"satisfy. Use only after a host OS update commits.")
	managerRootCmd.AddCommand(cleanupCmd)
}

func ExecuteManager(ctx context.Context) error {
	return managerRootCmd.ExecuteContext(ctx)
}
