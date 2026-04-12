package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
)

// hookTimeout bounds how long a single hook may run. containerd's own
// lifecycle timeouts are tighter than this, but a local ceiling keeps a
// misbehaving hook from wedging the runtime in tests or direct invocations.
const hookTimeout = 60 * time.Second

// ExecuteIfPresent runs a hook script from the extension rootfs if it exists.
// The hook path is relative to rootfs (e.g., "hooks/create").
// Returns nil if the hook does not exist.
func ExecuteIfPresent(logger *slog.Logger, rootfs string, hookPath string, annotations map[string]string) error {
	absPath := filepath.Join(rootfs, hookPath)

	info, err := os.Stat(absPath)
	if os.IsNotExist(err) {
		logger.Debug("hook not present, skipping", "hook", hookPath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to stat hook %s: %w", absPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("hook %s is a directory, not executable", absPath)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("hook %s is not executable", absPath)
	}

	logger.Info("executing hook", "hook", hookPath, "rootfs", rootfs)

	env := append(os.Environ(),
		"EXTENSION_ROOTFS="+rootfs,
	)
	env = append(env, labels.ToEnv(annotations)...)

	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, absPath)
	cmd.Env = env
	cmd.Stdout = os.Stderr // hooks log to runtime stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("hook %s timed out after %s", hookPath, hookTimeout)
		}
		return fmt.Errorf("hook %s failed: %w", hookPath, err)
	}

	logger.Info("hook completed", "hook", hookPath)
	return nil
}
