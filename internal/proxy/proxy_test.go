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

// unallocatablePid sits at or above PID_MAX_LIMIT on every architecture we
// build for: the kernel caps /proc/sys/kernel/pid_max at 4194304 on 64-bit
// and 32768 on 32-bit, then allocates strictly below that cap. No live
// process can carry this pid, so signalling it always fails with ESRCH.
const unallocatablePid = 4194304

// TestMain turns the test binary into a proxy stand-in.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "proxy":
			os.Exit(Run())
		case "slow":
			time.Sleep(300 * time.Millisecond)
			os.Exit(Run())
		case "exit-early":
			os.Exit(0)
		case "never-ready":
			// A timer, so the runtime reports no deadlock.
			time.Sleep(time.Hour)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// waitStatus reaps pid. spawn does not return the exec.Cmd, but the test
// process is still the parent.
func waitStatus(t *testing.T, pid int) syscall.WaitStatus {
	t.Helper()
	done := make(chan syscall.WaitStatus, 1)
	go func() {
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
		done <- ws
	}()
	select {
	case ws := <-done:
		return ws
	// Race builds sleep about 1s at a zero exit.
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-done
		t.Fatalf("process %d did not exit within 10s", pid)
		return 0
	}
}

// assertNoChildren fails if the test process has any child, live or zombie.
func assertNoChildren(t *testing.T) {
	t.Helper()
	var ws syscall.WaitStatus
	_, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
	assert.ErrorIs(t, err, syscall.ECHILD)
}

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
	assert.Error(t, Signal(unallocatablePid, syscall.SIGTERM))
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

// Without the handshake, a signal sent right after spawn reaches a child
// that does not listen yet: it dies by signal or ignores it.
func TestSpawn_WaitsForReadiness(t *testing.T) {
	tests := []struct {
		name   string
		sig    syscall.Signal
		status int
	}{
		{"SIGUSR1 reports success", syscall.SIGUSR1, 0},
		{"SIGUSR2 reports refusal", syscall.SIGUSR2, 1},
		{"SIGTERM stops", syscall.SIGTERM, 0},
		{"SIGINT stops", syscall.SIGINT, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, err := spawn(context.Background(), os.Args[0], "slow")
			require.NoError(t, err)

			require.NoError(t, Signal(pid, tt.sig))

			ws := waitStatus(t, pid)
			require.True(t, ws.Exited(), "proxy died by signal %v", ws.Signal())
			assert.Equal(t, tt.status, ws.ExitStatus())
		})
	}
}

func TestSpawn_ChildExitsBeforeReady(t *testing.T) {
	pid, err := spawn(context.Background(), os.Args[0], "exit-early")

	require.Error(t, err)
	assert.ErrorContains(t, err, "exited before it was ready")
	assert.Equal(t, -1, pid)
	assertNoChildren(t)
}

func TestSpawn_ReadinessTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	pid, err := spawn(ctx, os.Args[0], "never-ready")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, -1, pid)
	assertNoChildren(t)
}

// The readiness wait must disarm its kill once the proxy is ready.
func TestNewProcess_OutlivesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pid, err := NewProcess(ctx, "test-container")
	require.NoError(t, err)

	cancel()
	// Give a kill that was not disarmed time to land.
	time.Sleep(100 * time.Millisecond)

	require.NoError(t, Stop(pid))
	ws := waitStatus(t, pid)
	require.True(t, ws.Exited(), "proxy died by signal %v", ws.Signal())
	assert.Equal(t, 0, ws.ExitStatus())
}
