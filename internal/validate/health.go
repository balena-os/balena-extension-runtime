package validate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/override"
)

// The healthcheck binary sources board-specific checks and the VPN prestate,
// which is why it stays an exec.
const healthcheckBinary = "rollback-tests"

// Test seams over the healthcheck exec and the waits between attempts.
var (
	runHealthcheck = execHealthcheck
	wait           = sleep
)

// execHealthcheck runs one healthcheck attempt.
//
// The environment is built from scratch: this process inherits systemd's,
// and rollback-tests sources board scripts.
func execHealthcheck(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, healthcheckBinary)
	cmd.Env = []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"ROLLBACK_HEALTH_VARIABLES=" + override.HealthPrestatePath(),
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// healthchecksPass runs the healthchecks until one passes or the attempts run
// out. It gives up on the failed attempt rather than after sleeping through
// an interval that no further attempt follows.
//
// Only a check that ran and exited non-zero is the device's verdict; an error
// means it never ran.
func healthchecksPass(ctx context.Context, logger *slog.Logger, o Options) (bool, error) {
	for attempt := 1; ; attempt++ {
		err := runHealthcheck(ctx)
		if err == nil {
			return true, nil
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return false, fmt.Errorf("run %s: %w", healthcheckBinary, err)
		}
		if attempt >= o.Attempts {
			logger.Warn("healthchecks failed", "attempts", attempt, "err", err)
			return false, nil
		}
		logger.Info("retrying healthcheck", "attempt", attempt, "attempts", o.Attempts,
			"in", o.Retry, "err", err)
		if err := wait(ctx, o.Retry); err != nil {
			return false, err
		}
	}
}
