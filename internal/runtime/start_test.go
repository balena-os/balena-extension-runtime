package runtime

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var startTestLogger = slog.New(slog.NewTextHandler(os.Stderr, nil))

// fakeStartProxy swaps the start/fail signal seams and records which one
// fired, so the routing decision can be asserted without a live proxy.
type fakeStartProxy struct {
	started []int
	failed  []int
}

func (f *fakeStartProxy) install(t *testing.T) {
	t.Helper()
	prevStart, prevFail := proxyStart, proxyFail
	proxyStart = func(pid int) error {
		f.started = append(f.started, pid)
		return nil
	}
	proxyFail = func(pid int) error {
		f.failed = append(f.failed, pid)
		return nil
	}
	t.Cleanup(func() {
		proxyStart = prevStart
		proxyFail = prevFail
	})
}

// writeCreatedState persists a Created state for the bundle, standing in for
// what a prior create call would have left behind.
func writeCreatedState(t *testing.T, containerID, bundle string) {
	t.Helper()
	state := oci.NewState(containerID, bundle)
	state.Status = specs.StateCreated
	state.Pid = 12345
	require.NoError(t, oci.WriteState(state))
}

// A hook that runs and fails is the extension refusing its activation. The
// verdict must travel as the container's exit status: start succeeds, the
// proxy is told to exit non-zero, and the state records a stopped container.
// Failing the start call instead would flatten the verdict into engine error
// prose and leave callers retrying an image that can never activate.
func TestStart_RejectedHookSucceedsStartAndFailsContainer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	bundle := validBundleWithAnnotations(t)
	hookDir := filepath.Join(bundle, "rootfs", "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "start"),
		[]byte("#!/bin/sh\nexit 1\n"), 0o755))

	containerID := "start-reject-test"
	writeCreatedState(t, containerID, bundle)
	fake := &fakeStartProxy{}
	fake.install(t)

	require.NoError(t, Start(startTestLogger, containerID))

	assert.Equal(t, []int{12345}, fake.failed, "proxy must be told to exit non-zero")
	assert.Empty(t, fake.started, "the clean-exit signal must not fire on a rejection")

	state, err := oci.ReadState(containerID)
	require.NoError(t, err)
	assert.Equal(t, specs.StateStopped, state.Status)
}

// The success path is the same shape with the other signal.
func TestStart_CleanHookSignalsCleanExit(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	bundle := validBundleWithAnnotations(t)

	containerID := "start-clean-test"
	writeCreatedState(t, containerID, bundle)
	fake := &fakeStartProxy{}
	fake.install(t)

	require.NoError(t, Start(startTestLogger, containerID))

	assert.Equal(t, []int{12345}, fake.started)
	assert.Empty(t, fake.failed)

	state, err := oci.ReadState(containerID)
	require.NoError(t, err)
	assert.Equal(t, specs.StateStopped, state.Status)
}

// A failure that says nothing about the extension must keep failing the start
// call, leaving the container created and the attempt retryable. An unreadable
// spec is such a failure; a rejection routed here by mistake would surface as
// a retry loop, the exact defect this split removes.
func TestStart_RuntimeFailureStillFailsTheStartCall(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	bundle := validBundleWithAnnotations(t)
	// Corrupt the spec so the failure happens before any hook is consulted.
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), []byte("not json"), 0o644))

	containerID := "start-runtime-fail-test"
	writeCreatedState(t, containerID, bundle)
	fake := &fakeStartProxy{}
	fake.install(t)

	require.Error(t, Start(startTestLogger, containerID))

	assert.Empty(t, fake.started)
	assert.Empty(t, fake.failed)

	state, err := oci.ReadState(containerID)
	require.NoError(t, err)
	assert.Equal(t, specs.StateCreated, state.Status, "a retryable failure must leave the container created")
}
