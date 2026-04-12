package proxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// NewProcess spawns a proxy subprocess that blocks until signaled.
// Returns the PID of the child process. The context bounds how long we are
// willing to wait for the spawn itself; once Start returns the proxy lives
// independently (Setpgid detaches it from our process group).
//
// Liveness invariant: the proxy has no self-timeout and relies on an
// external Delete to terminate it. If containerd or its shim dies mid-
// lifecycle, recovery happens via containerd's cleanupAfterDeadShim path
// (shim reattach on startup, or ttrpc onClose when a shim crashes under
// a live containerd), which re-execs the shim binary with the delete
// action — which in turn invokes our runtime's delete and sends SIGTERM
// to this proxy. The invariant therefore requires the shim binary to be
// present and functional at cleanup time.
func NewProcess(ctx context.Context, containerID string) (int, error) {
	execPath, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("failed to get executable path: %w", err)
	}

	cmd := exec.CommandContext(ctx, execPath, "proxy", "--id", containerID)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	// CommandContext kills the process when ctx is cancelled; unset the
	// cancel hook so the long-lived proxy isn't torn down when the
	// create-call context ends. Spawn still respects ctx via Start.
	cmd.Cancel = func() error { return nil }
	// The proxy is our own binary and only blocks on signals, so it needs
	// no env. Set an explicit minimal environment rather than inheriting
	// os.Environ() — the runtime process env may carry containerd auth
	// tokens / TTRPC addresses, and even though the proxy is trusted code,
	// anything exec'd later under this process inherits too. A bounded PATH
	// keeps the door closed if that ever changes.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("failed to start proxy process: %w", err)
	}

	return cmd.Process.Pid, nil
}

// Signal sends a signal to the proxy process by PID.
func Signal(pid int, sig syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}
	if err := process.Signal(sig); err != nil {
		return fmt.Errorf("failed to send %s to %d: %w", sig, pid, err)
	}
	return nil
}

// Start tells the proxy to proceed (SIGUSR1 → proxy exits cleanly).
func Start(pid int) error {
	return Signal(pid, syscall.SIGUSR1)
}

// Stop terminates the proxy (SIGTERM).
func Stop(pid int) error {
	return Signal(pid, syscall.SIGTERM)
}
