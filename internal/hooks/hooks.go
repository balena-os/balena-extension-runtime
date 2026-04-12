package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
)

// hookTimeout bounds how long a single hook may run. containerd's own
// lifecycle timeouts are tighter than this, but a local ceiling keeps a
// misbehaving hook from wedging the runtime in tests or direct invocations.
const hookTimeout = 60 * time.Second

// hookEnvPath is the sanitized PATH passed to hook subprocesses. A fixed
// value prevents a quirky PATH in the containerd environment from
// redirecting hook binary lookups, and removes one way a caller's
// environment could influence execution.
const hookEnvPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// ExecuteIfPresent runs a hook script from the extension rootfs if it exists.
// The hook path is relative to rootfs (e.g., "hooks/create").
// Returns nil if the hook does not exist.
//
// Extension images are assumed trusted, so we do not defend against
// symlinks under hooks/ redirecting execution to arbitrary host binaries.
func ExecuteIfPresent(logger *slog.Logger, rootfs string, hookPath string, annotations map[string]string) error {
	if filepath.IsAbs(hookPath) {
		return fmt.Errorf("hook path %q must be relative to rootfs", hookPath)
	}
	cleaned := filepath.Clean(hookPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("hook path %q escapes rootfs", hookPath)
	}
	absPath := filepath.Join(rootfs, cleaned)

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

	// Build the hook environment from scratch rather than inheriting
	// os.Environ(): the runtime runs with the full containerd process env,
	// which can include auth tokens, TTRPC addresses, and API credentials.
	// A hook script is extension-provided code and must not receive those.
	// The contract is documented: hooks see PATH, EXTENSION_ROOTFS, and
	// the extension label env (labels.ToEnv).
	env := []string{
		"PATH=" + hookEnvPath,
		"EXTENSION_ROOTFS=" + rootfs,
	}
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
