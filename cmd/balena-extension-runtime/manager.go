package main

import (
	"context"
	"fmt"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/manager"
	"github.com/balena-os/balena-extension-runtime/internal/validate"
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

// The healthcheck waits, in seconds. Flags rather than environment, so the
// unit line shows what a device is waiting for.
var (
	settleSeconds  uint
	retrySeconds   uint
	healthAttempts uint
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Judge a kernel override armed outside a host OS update",
	Long: "Forget every override record no deployed extension claims, then " +
		"commit, reject or leave pending the override this boot armed. " +
		"A device with no boot environment block has no override axis and " +
		"exits zero.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return validate.Run(cmd.Context(), logger, validate.Options{
			Settle:   time.Duration(settleSeconds) * time.Second,
			Retry:    time.Duration(retrySeconds) * time.Second,
			Attempts: int(healthAttempts),
		})
	},
}

var hupCmd = &cobra.Command{
	Use:   "hup",
	Short: "Conclude a kernel override window a host OS update owns",
}

// Neither subcommand takes a slot: each derives its own, and writing the
// wrong slot's committed value is how a proven kernel gets retired.
var hupCommitCmd = &cobra.Command{
	Use:   "commit",
	Short: "Record the running kernel as the running slot's proven override",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return validate.HUPCommit(cmd.Context(), logger)
	},
}

var hupRejectCmd = &cobra.Command{
	Use:   "reject",
	Short: "Undo the active override in favour of the slot being rolled into",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return validate.HUPReject(cmd.Context(), logger)
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
	validateCmd.Flags().UintVar(&settleSeconds, "settle", 60,
		"Seconds to wait before the first healthcheck")
	validateCmd.Flags().UintVar(&retrySeconds, "retry", 60,
		"Seconds to wait between healthcheck attempts")
	validateCmd.Flags().UintVar(&healthAttempts, "attempts", 15,
		"Healthcheck attempts before the override is rejected")

	hupCmd.AddCommand(hupCommitCmd, hupRejectCmd)
	managerRootCmd.AddCommand(cleanupCmd, validateCmd, hupCmd)
}

func ExecuteManager(ctx context.Context) error {
	return managerRootCmd.ExecuteContext(ctx)
}
