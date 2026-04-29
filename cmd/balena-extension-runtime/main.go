package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	// Translate SIGTERM/SIGINT (typically from containerd cancelling a
	// runtime invocation) into context cancellation so subcommands holding
	// cmd.Context() can abort in-flight work cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var err error
	switch filepath.Base(os.Args[0]) {
	case "balena-extension-manager":
		err = ExecuteManager(ctx)
	default:
		err = Execute(ctx)
	}
	CloseLogger()
	if err != nil {
		os.Exit(1)
	}
}
