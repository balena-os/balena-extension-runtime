package runtime

import (
	"syscall"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unallocatablePid sits at or above PID_MAX_LIMIT on every architecture we
// build for: the kernel caps /proc/sys/kernel/pid_max at 4194304 on 64-bit
// and 32768 on 32-bit, then allocates strictly below that cap. No live
// process can carry this pid, so signalling it always fails with ESRCH.
const unallocatablePid = 4194304

// TestKill_MissingProcessIsToleratedForTermination asserts that reaping a task
// whose proxy already exited does not fail the teardown. Both signals Kill
// tolerates are covered: containerd sends SIGTERM on the task-kill path before
// the force-delete sends SIGKILL, so narrowing the tolerance to SIGKILL alone
// would break stop while leaving delete green.
func TestKill_MissingProcessIsToleratedForTermination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{"SIGKILL", syscall.SIGKILL},
		{"SIGTERM", syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

			const id = "kill-gone-test"
			state := oci.NewState(id, t.TempDir())
			state.Pid = unallocatablePid
			state.Status = specs.StateRunning
			require.NoError(t, oci.WriteState(state))

			// Kill swallows delivery failures for both signals under test, so
			// none of the assertions below can tell a missing process from a
			// live one. Pin the precondition here instead: were this pid ever
			// allocatable, the test would signal an unrelated process and
			// still report success.
			require.Error(t, syscall.Kill(state.Pid, 0), "pid under test must not be live")

			assert.NoError(t, Kill(testLogger(), id, tc.signal))

			stopped, err := oci.ReadState(id)
			require.NoError(t, err)
			assert.Equal(t, specs.StateStopped, stopped.Status)
		})
	}
}
