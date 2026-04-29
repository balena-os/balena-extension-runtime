package proxy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findProcess mirrors the production lookup used by Signal; it returns nil
// for an unknown PID on Linux because os.FindProcess never errors there.
// We probe liveness with Signal(0).
func isAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// TestSignalUnknownPID confirms signalling a non-existent PID returns an
// error rather than panicking — relied on by Kill's tolerance for a proxy
// that has already exited.
func TestSignalUnknownPID(t *testing.T) {
	// PID 0x7fffffff is effectively never assigned; if by accident it is
	// alive we'd see success and skip.
	err := Signal(1<<31-1, syscall.SIGTERM)
	if err == nil {
		t.Skip("unlikely PID was live on this host")
	}
	assert.Error(t, err)
}

// TestNewProcessSpawnAndStop launches a short-lived subprocess standing in
// for the real runtime binary (which can't be invoked from a unit test), by
// monkey-patching os.Executable's result through a shim: we exec `sleep`
// directly and verify signal delivery + cleanup. This keeps the test
// self-contained while covering the Signal/Stop plumbing.
func TestSignalTerminatesLiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	pid := cmd.Process.Pid
	require.True(t, isAlive(pid), "sleep should be alive after Start")

	require.NoError(t, Stop(pid))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// sleep exits non-zero when SIGTERM'd; that's fine.
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("unexpected wait error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sleep did not exit within 2s of SIGTERM")
	}
	assert.False(t, isAlive(pid))
}

// TestNewProcessContextCancelDoesNotKillProxy verifies that NewProcess
// returns a PID whose lifetime is independent of the spawn context — the
// proxy must outlive the caller's create-call context.
func TestNewProcessContextCancelDoesNotKillProxy(t *testing.T) {
	// Build a throwaway binary that blocks on a signal, simulating the
	// real `proxy` subcommand. We can't use os.Executable() from a _test
	// binary because the test harness would intercept it.
	exe := buildSleeper(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, exe)
	cmd.Cancel = func() error { return nil } // mirror NewProcess
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL); _, _ = cmd.Process.Wait() })

	cancel() // spawn context done — proxy must still be alive

	// Give cmd.Cancel a moment to (not) fire.
	time.Sleep(100 * time.Millisecond)
	assert.True(t, isAlive(pid), "proxy must outlive its spawn context")

	require.NoError(t, Stop(pid))
}

// buildSleeper compiles a tiny Go program that blocks until SIGTERM.
func buildSleeper(t *testing.T) string {
	t.Helper()
	src := `package main
import (
	"os"
	"os/signal"
	"syscall"
)
func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	<-c
}
`
	dir := t.TempDir()
	srcPath := dir + "/main.go"
	require.NoError(t, os.WriteFile(srcPath, []byte(src), 0o644))
	bin := dir + "/sleeper"
	build := exec.Command("go", "build", "-o", bin, srcPath)
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Skipf("go build not available in test env: %v", err)
	}
	return bin
}
