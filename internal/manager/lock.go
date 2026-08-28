package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// opMu is a size-1 channel rather than a sync.Mutex because a caller must be
// able to give up waiting, and sync.Mutex.Lock cannot be abandoned.
var opMu = make(chan struct{}, 1)

// lockPath is the file the cross-process lock is taken on. /run is tmpfs, so
// no lock file survives a reboot, and the kernel releases an flock when its
// holder dies, which is what a boot-time unit killed mid-operation relies on.
// A variable so tests can point it somewhere writable.
var lockPath = "/run/balena-extension-manager.lock"

// lockPollInterval paces the retry loop standing in for a blocking flock.
// The operations it waits on run for seconds at minimum, so polling this
// slowly costs nothing measurable against the wait it is spent in.
const lockPollInterval = 50 * time.Millisecond

// WithOperationLock runs fn under both layers, waiting until they are free or
// ctx is done. It is the only site that acquires either one, so they cannot be
// taken in opposite orders, and fn cannot run without both.
func WithOperationLock(ctx context.Context, fn func() error) error {
	select {
	case opMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-opMu }()

	f, err := acquireFileLock(ctx)
	if err != nil {
		return err
	}
	// Closing the descriptor is what releases the flock. The file itself is
	// deliberately left behind: unlinking it would race a process that has
	// already opened the old inode, leaving two holders locked against
	// different inodes and neither aware of the other.
	defer f.Close()

	// Neither wait above rejects a context that is already done: the select
	// has two ready cases when the lock is free, and a non-blocking flock
	// succeeds without consulting ctx. Re-check here so a cancelled caller
	// cannot reach fn, rather than resting on every implementation
	// short-circuiting on its own first engine call.
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// acquireFileLock returns the descriptor holding the flock on lockPath. A
// blocking LOCK_EX cannot be abandoned once the kernel has parked the thread,
// so the wait is spelled out as non-blocking attempts paced against ctx.
func acquireFileLock(ctx context.Context) (*os.File, error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		timer := time.NewTimer(lockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
