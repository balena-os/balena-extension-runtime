package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// The proxy writes its readiness byte here.
const readyFD = 3

// NewProcess spawns a proxy subprocess that blocks until signaled.
// It returns the PID only after the proxy listens for SIGUSR1, SIGUSR2,
// SIGTERM and SIGINT. ctx bounds that wait. If the proxy dies or ctx ends
// first, NewProcess kills and reaps the proxy and returns an error.
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
	return spawn(ctx, execPath, "proxy", "--id", containerID)
}

func spawn(ctx context.Context, path string, args ...string) (int, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return -1, fmt.Errorf("failed to create proxy readiness pipe: %w", err)
	}
	defer r.Close()

	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	// The proxy is our own binary and only blocks on signals, so it needs
	// no env. Set an explicit minimal environment rather than inheriting
	// os.Environ() — the runtime process env may carry containerd auth
	// tokens / TTRPC addresses, and even though the proxy is trusted code,
	// anything exec'd later under this process inherits too. A bounded PATH
	// keeps the door closed if that ever changes.
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.ExtraFiles = []*os.File{w} // readyFD in the child

	startErr := cmd.Start()
	// The proxy must hold the last write end, so its death gives EOF.
	_ = w.Close()
	if startErr != nil {
		return -1, fmt.Errorf("failed to start proxy process: %w", startErr)
	}

	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	_, readErr := r.Read(make([]byte, 1))
	if stop() && readErr == nil {
		return cmd.Process.Pid, nil
	}
	// Not reaped yet, so the kill cannot hit a reused PID.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, fmt.Errorf("proxy not ready: %w", ctxErr)
	}
	return -1, errors.New("proxy exited before it was ready")
}

// Run is the proxy process. It returns the exit status, which the engine
// records as the container's verdict: SIGUSR1 reports a successful activation
// (0), SIGUSR2 reports a refusal (1), SIGTERM and SIGINT are normal stops (0).
func Run() int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGTERM, syscall.SIGINT)
	// Report ready only after Notify installs the handlers.
	if err := reportReady(); err != nil {
		// spawn sees EOF and fails create, so no verdict exists.
		return 1
	}
	if <-sigCh == syscall.SIGUSR2 {
		return 1
	}
	return 0
}

func reportReady() error {
	f := os.NewFile(readyFD, "ready")
	_, err := f.Write([]byte{1})
	_ = f.Close()
	return err
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

// Start tells the proxy the activation succeeded (SIGUSR1 → proxy exits 0).
func Start(pid int) error {
	return Signal(pid, syscall.SIGUSR1)
}

// Fail tells the proxy the extension refused its activation (SIGUSR2 → proxy
// exits 1).
func Fail(pid int) error {
	return Signal(pid, syscall.SIGUSR2)
}

// Stop terminates the proxy (SIGTERM).
func Stop(pid int) error {
	return Signal(pid, syscall.SIGTERM)
}
