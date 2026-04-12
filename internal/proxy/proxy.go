package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// NewProcess spawns a proxy subprocess that blocks until signaled.
// Returns the PID of the child process.
func NewProcess(containerID string) (int, error) {
	execPath, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("failed to get executable path: %w", err)
	}

	cmd := exec.Command(execPath, "proxy", "--id", containerID)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

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
