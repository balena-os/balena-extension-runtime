package validate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/balena-os/balena-extension-runtime/internal/override"
)

// HUPCommit records the running kernel as the running slot's proven override,
// at the end of a healthy host OS update.
//
// The health prestate is left alone, as rollback-health leaves it; the next
// arm replaces it.
func HUPCommit(ctx context.Context, logger *slog.Logger) error {
	env, err := readBlockOrSkip(logger, "nothing to commit")
	if err != nil || env == nil {
		return err
	}

	armed, _ := env.Get(bootenv.KeyABI)
	running, err := runningABI()
	if err != nil {
		return err
	}
	if armed == "" || armed != running {
		logger.Info("no kernel override took effect; nothing to commit",
			"abi", armed, "running", running)
		return nil
	}
	slot, err := RunningSlot()
	if err != nil {
		return fmt.Errorf("naming the running slot: %w", err)
	}

	logger.Info("committing the kernel override", "abi", armed, "slot", slot)
	return withOperationLock(ctx, func() error {
		wrote, err := bootenv.Commit(armed, slot)
		if err != nil {
			return err
		}
		if !wrote {
			logger.Warn("the validation window moved; not committing", "abi", armed)
		}
		return nil
	})
}

// HUPReject undoes the active override in favour of the slot the rollback
// lands in, and relays the rejection to the next boot rather than undoing the
// records inline, because the reboot follows immediately.
//
// It does not record the ABI as rejected: the rootfs rolled back, so nothing
// was proven about the kernel bytes on the OS they were built for.
//
// The audit line goes first, fsynced, and is written for any nonempty arm,
// including a redundant rollback that relays nothing. An empty arm writes
// neither.
func HUPReject(ctx context.Context, logger *slog.Logger) error {
	env, err := readBlockOrSkip(logger, "nothing to undo")
	if err != nil || env == nil {
		return err
	}

	armed, _ := env.Get(bootenv.KeyABI)
	if armed == "" {
		logger.Info("no kernel override armed; nothing to undo")
		return nil
	}
	running, err := runningABI()
	if err != nil {
		return err
	}
	slot, err := RunningSlot()
	if err != nil {
		return fmt.Errorf("naming the running slot: %w", err)
	}
	target := slot.Other()

	to, _ := env.Get(bootenv.KeyCommitted(target))
	logger.Info("undoing the kernel override", "abi", armed, "slot", target, "to", to)

	return withOperationLock(ctx, func() error {
		line := override.Line{By: "health", From: armed, To: to, Slot: string(target)}
		if err := override.WriteAuditLine(line); err != nil {
			return err
		}
		relayed, err := bootenv.HUPReject(target, running)
		if err != nil {
			return err
		}
		if !relayed {
			logger.Info("the arm was already this slot's proven override; relaying nothing",
				"abi", armed, "slot", target)
		}
		return nil
	})
}

// readBlockOrSkip returns nil, nil on a device that has no block, which is
// not a defect: it has no override axis.
func readBlockOrSkip(logger *slog.Logger, what string) (*bootenv.Env, error) {
	env, err := bootenv.Read()
	if errors.Is(err, bootenv.ErrNoBlock) {
		logger.Info("no boot environment block; "+what, "path", bootenv.Path())
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return env, nil
}
