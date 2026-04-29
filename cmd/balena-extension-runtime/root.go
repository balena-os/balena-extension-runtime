package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/balena-os/balena-extension-runtime/internal/log"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/balena-os/balena-extension-runtime/internal/version"
	"github.com/spf13/cobra"
)

var (
	logLevel   string
	logFile    string
	logFormat  string
	stateRoot  string
	dockerRoot string
	logger     *slog.Logger
	logCloser  io.Closer
)

var rootCmd = &cobra.Command{
	Use:          "balena-extension-runtime",
	Short:        "OCI runtime for balenaOS hostapp extensions",
	Version:      fmt.Sprintf("%s (commit: %s)", version.Version, version.GitCommit),
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := initLogger(); err != nil {
			return err
		}
		if stateRoot != "" {
			oci.SetStateRoot(stateRoot)
		}
		if dockerRoot != "" {
			oci.SetDockerRoot(dockerRoot)
		}
		return nil
	},
}

func Execute(ctx context.Context) error {
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info",
		"Set the logging level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVar(&stateRoot, "root", "",
		"Root directory for runtime state (passed by containerd)")
	rootCmd.PersistentFlags().StringVar(&logFile, "log", "",
		"Log file path for runtime events (passed by containerd)")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text",
		"Log format: text or json")
	rootCmd.PersistentFlags().StringVar(&dockerRoot, "docker-root", "/var/lib/docker",
		"Docker data root directory for label fallback via config.v2.json")
	// --systemd-cgroup is passed by containerd; accepted and ignored (extensions
	// are short-lived proxy processes and do not require cgroup delegation).
	rootCmd.PersistentFlags().Bool("systemd-cgroup", false,
		"Use systemd cgroup driver (passed by containerd; ignored)")
}

func initLogger() error {
	level, err := parseLogLevel(logLevel)
	if err != nil {
		return err
	}
	logger, logCloser, err = log.NewLoggerWithFile(level, logFile, logFormat)
	return err
}

// CloseLogger releases any resources held by the logger (e.g. the log file
// opened by --log). Safe to call if the logger was never initialised.
func CloseLogger() {
	if logCloser != nil {
		_ = logCloser.Close()
	}
}

func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level %q", level)
	}
}
