// Package validate judges a kernel override: Run on an ordinary boot,
// HUPCommit and HUPReject at the two verdicts of a host OS update window.
//
// Four records name an ABI: the armed value in the boot environment block,
// the committed value of each slot, the kernel published under boot-by-abi,
// and the claim a deployed extension makes on it. Run keeps the first three
// in step with the fourth, then puts an armed override on trial.
package validate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/balena-os/balena-extension-runtime/internal/manager"
	"github.com/balena-os/balena-extension-runtime/internal/override"
	"github.com/balena-os/hostapp"
)

// trialLimit is the boot budget an armed override gets before the spent count
// is the verdict. It must agree with TRIAL_LIMIT in the initramfs kexec
// script; both copies ship in one rootfs image and are swapped together.
const trialLimit = 3

// Options are the healthcheck waits, spelled out on the unit line.
type Options struct {
	Settle   time.Duration
	Retry    time.Duration
	Attempts int
}

// Where the running kernel's ABI is stamped. A variable so tests can
// redirect it.
var procCmdline = "/proc/cmdline"

// Test seams over the reboot a health rejection ends in and over the
// cross-process lock, whose real home is /run.
var (
	reboot            = systemctlReboot
	withOperationLock = manager.WithOperationLock
)

// Run is the ordinary-boot trial and the record sweep.
func Run(ctx context.Context, logger *slog.Logger, o Options) error {
	env, err := readBlockOrSkip(logger, "nothing to validate")
	if err != nil || env == nil {
		return err
	}

	if relayed, _ := env.Get(bootenv.KeyRejected); relayed != "" {
		logger.Warn("kernel override was undone by a host OS rollback; forgetting it", "abi", relayed)
		if err := consumeRelay(ctx, relayed); err != nil {
			return err
		}
		if env, err = bootenv.Read(); err != nil {
			return err
		}
	}

	if v, _ := env.Get(bootenv.KeyUpgradeAvailable); v == "1" {
		logger.Info("host OS update in progress; leaving the override to the update path")
		return nil
	}

	swept, err := sweep(ctx, logger, env)
	if err != nil {
		return err
	}
	if swept {
		if env, err = bootenv.Read(); err != nil {
			return err
		}
	}

	slot, err := RunningSlot()
	if err != nil {
		return fmt.Errorf("naming the running slot: %w", err)
	}

	armed, _ := env.Get(bootenv.KeyABI)
	committed, _ := env.Get(bootenv.KeyCommitted(slot))
	if armed == committed {
		return nil
	}

	running, err := runningABI()
	if err != nil {
		return err
	}

	// Not a verdict: the sweep, not this row, retires records.
	if armed == "" {
		logger.Warn("no override armed beside a proven one; restoring it",
			"slot", slot, "abi", committed)
		return restore(ctx, logger, slot)
	}

	if armed != running {
		return pending(ctx, logger, env, armed, slot, running)
	}

	logger.Info("kernel override is on trial", "abi", armed, "settle", o.Settle)
	if err := wait(ctx, o.Settle); err != nil {
		return err
	}
	passed, err := healthchecksPass(ctx, logger, o)
	if err != nil {
		// Bounded: pending rejects the arm once the boot budget is spent.
		return err
	}
	if passed {
		logger.Info("override proved healthy; committing it", "abi", armed, "slot", slot)
		return commit(ctx, logger, armed, slot)
	}
	if err := reject(ctx, logger, env, armed, slot, "health", ""); err != nil {
		return err
	}
	// The arm names this slot's committed override again, but the running
	// kernel is the rejected one, so the device has to leave it.
	return reboot()
}

// pending judges an arm that did not take effect. Nothing new was tested, so
// neither committing nor undoing it is right until one of two things is.
func pending(ctx context.Context, logger *slog.Logger, env *bootenv.Env,
	armed string, slot bootenv.Slot, running string) error {
	published, err := override.KernelPublished(armed)
	if err != nil {
		return err
	}
	if !published {
		// The sweep above forgot every unclaimed ABI, so an arm that survived
		// it is claimed and a missing kernel can only be a defect rather than
		// a verdict. Nothing was tested, so nothing is recorded.
		logger.Warn("override is claimed but no kernel is published; forgetting it",
			"abi", armed, "dir", override.BootByABIDir)
		return forget(ctx, armed)
	}

	// The count is read only here. On the row above nothing was tested, and
	// on the running-arm path the kernel is still being judged.
	if boots := trialCount(env); boots >= trialLimit {
		// Stage 2 stops offering an arm at the limit, so without this the row
		// would hold it pending for the life of the device. No reboot: the
		// restored arm names the kernel this boot is already running.
		return reject(ctx, logger, env, armed, slot, "boots", strconv.Itoa(boots))
	}
	logger.Info("override did not boot; leaving it pending", "abi", armed, "running", running)
	return nil
}

// consumeRelay closes the window a host OS rollback rejected, recording
// nothing against the kernel bytes: the rootfs rolled back, so nothing was
// proven about them.
func consumeRelay(ctx context.Context, abi string) error {
	return withOperationLock(ctx, func() error {
		if err := bootenv.ConsumeRelay(abi); err != nil {
			return err
		}
		return errors.Join(override.RemoveKernel(abi), override.RemoveHealthPrestate())
	})
}

// forget closes a window without a verdict. Block first: a link no record
// names is collected by the next sweep, where the opposite order costs a boot.
func forget(ctx context.Context, abi string) error {
	return withOperationLock(ctx, func() error {
		if _, err := bootenv.Forget([]string{abi}); err != nil {
			return err
		}
		return errors.Join(override.RemoveKernel(abi), override.RemoveHealthPrestate())
	})
}

// commit records abi as slot's proven override. Block first: a crash leaves
// a prestate for a closed window, which the next arm replaces through the
// temporary-name rename activation already uses.
func commit(ctx context.Context, logger *slog.Logger, abi string, slot bootenv.Slot) error {
	return withOperationLock(ctx, func() error {
		wrote, err := bootenv.Commit(abi, slot)
		if err != nil {
			return err
		}
		if !wrote {
			// Activation rewrote the prestate when it armed the new window,
			// and removing it would strip that trial of its baseline.
			logger.Warn("the validation window moved; not committing", "abi", abi)
			return nil
		}
		return override.RemoveHealthPrestate()
	})
}

// restore points a cleared arm back at the slot's proven override. No window
// opened, so there is no prestate to remove and no reboot to do.
func restore(ctx context.Context, logger *slog.Logger, slot bootenv.Slot) error {
	return withOperationLock(ctx, func() error {
		wrote, err := bootenv.Restore(slot)
		if err != nil {
			return err
		}
		if !wrote {
			// A deploy armed one meanwhile; that arm owns its boot.
			logger.Warn("an override was armed meanwhile; leaving it alone", "slot", slot)
		}
		return nil
	})
}

// reject records that this boot proved the override unusable, then undoes it.
//
// The records go first, both fsynced. The rejection record is what refuses
// the arm when helios redeploys, so a crash that lost it while the arm was
// already gone would let the same kernel bytes be armed again with nothing
// recording that they failed.
func reject(ctx context.Context, logger *slog.Logger, env *bootenv.Env,
	abi string, slot bootenv.Slot, by, boots string) error {
	// Read before the block write erases it.
	to, _ := env.Get(bootenv.KeyCommitted(slot))
	logger.Warn("kernel override rejected; undoing it", "abi", abi, "by", by, "slot", slot)

	return withOperationLock(ctx, func() error {
		if err := override.RecordRejection(abi); err != nil {
			return err
		}
		line := override.Line{By: by, From: abi, To: to, Slot: string(slot), Boots: boots}
		if err := override.WriteAuditLine(line); err != nil {
			return err
		}

		wrote, err := bootenv.Reject(abi, slot)
		if err != nil {
			return err
		}
		if err := override.RemoveKernel(abi); err != nil {
			return err
		}
		if !wrote {
			// The judged kernel bytes failed whatever was armed afterwards,
			// so the records stand; the new window keeps its prestate.
			logger.Warn("the validation window moved; the records stand and the arm is left alone",
				"abi", abi)
			return nil
		}
		return override.RemoveHealthPrestate()
	})
}

// trialCount reads the boot budget stage 2 has spent. An unset or non-numeric
// key reads as zero, which is how stage 2 reads it too.
func trialCount(env *bootenv.Env) int {
	value, _ := env.Get(bootenv.KeyTrial)
	boots, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || boots < 0 {
		return 0
	}
	return boots
}

// runningABI is the kernel this boot actually loaded, empty for stock. The
// initramfs stamps the token, which is the only account of which kernel is
// running as opposed to which one was asked for.
func runningABI() (string, error) {
	cmdline, err := os.ReadFile(procCmdline)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", procCmdline, err)
	}
	return hostapp.ParseHostKernelABIID(string(cmdline)), nil
}

// systemctlReboot leaves the rejected kernel. Not reboot(2): the clean
// shutdown is what flushes the journal, and every write above is fsynced.
func systemctlReboot() error {
	cmd := exec.Command("systemctl", "reboot")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
