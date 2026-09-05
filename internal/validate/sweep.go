package validate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/balena-os/balena-extension-runtime/internal/bootenv"
	"github.com/balena-os/balena-extension-runtime/internal/override"
	"github.com/balena-os/hostapp"
)

// The on-disk container store, not the engine's list: this unit runs with the
// engine possibly down, and its claim set has to be the one the initramfs
// acts on. A variable so tests can redirect it.
var dataRoot = "/mnt/data/docker"

// Test seam over the claim query.
var claimedABIs = hostapp.ClaimedKernelABIs

// sweep forgets every recorded ABI no deployed extension claims, and reports
// whether it wrote. Nothing else collects what a withdrawn extension leaves.
//
// Two orderings are load-bearing. The recorded set is read before the claim
// query, so a deploy landing mid-sweep is never judged. The query runs inside
// the lock the runtime's create takes, which is what covers a redeploy of an
// ABI the set already named.
func sweep(ctx context.Context, logger *slog.Logger, env *bootenv.Env) (bool, error) {
	recorded, err := recordedABIs(env)
	if err != nil {
		return false, err
	}
	// Cost control: no record means no store read and no lock.
	if len(recorded) == 0 {
		return false, nil
	}

	wrote := false
	err = withOperationLock(ctx, func() error {
		unclaimed := unclaimedABIs(logger, recorded)
		if len(unclaimed) == 0 {
			return nil
		}
		logger.Info("no deployed extension claims these kernel overrides; forgetting them",
			"abis", unclaimed)

		// One write for N ABIs. The block first: a link no record names is
		// collected by the next sweep, where the opposite order costs a boot.
		armCleared, err := bootenv.Forget(unclaimed)
		if err != nil {
			return fmt.Errorf("forget unclaimed kernel overrides: %w", err)
		}
		wrote = true

		var errs []error
		for _, abi := range unclaimed {
			if err := override.RemoveKernel(abi); err != nil {
				errs = append(errs, err)
			}
		}
		// Sweeping the arm closes its window.
		if armCleared {
			errs = append(errs, override.RemoveHealthPrestate())
		}
		return errors.Join(errs...)
	})
	return wrote, err
}

// unclaimedABIs is the members of recorded that no deployed extension claims.
//
// A failed query yields nothing to sweep. "Cannot tell" must never read as an
// empty claim set, which would forget every ABI on a data partition that is
// not yet populated.
func unclaimedABIs(logger *slog.Logger, recorded []string) []string {
	claimed, err := claimedABIs(dataRoot)
	if err != nil {
		logger.Warn("cannot determine the deployed extensions; leaving the override records alone",
			"err", err)
		return nil
	}

	claims := make(map[string]struct{}, len(claimed))
	for _, abi := range claimed {
		claims[abi] = struct{}{}
	}
	var unclaimed []string
	for _, abi := range recorded {
		if _, ok := claims[abi]; !ok {
			unclaimed = append(unclaimed, abi)
		}
	}
	return unclaimed
}

// recordedABIs is every ABI this device has a record of: the arm, either
// slot's committed value, and the published kernels.
func recordedABIs(env *bootenv.Env) ([]string, error) {
	set := map[string]struct{}{}
	add := func(abi string) {
		if abi != "" {
			set[abi] = struct{}{}
		}
	}
	arm, _ := env.Get(bootenv.KeyABI)
	add(arm)
	for _, slot := range []bootenv.Slot{bootenv.SlotA, bootenv.SlotB} {
		committed, _ := env.Get(bootenv.KeyCommitted(slot))
		add(committed)
	}
	published, err := override.ListPublished()
	if err != nil {
		return nil, err
	}
	for _, abi := range published {
		add(abi)
	}

	out := make([]string, 0, len(set))
	for abi := range set {
		out = append(out, abi)
	}
	sort.Strings(out)
	return out, nil
}
