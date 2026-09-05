package runtime

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/balena-os/balena-extension-runtime/internal/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var startTestLogger = slog.New(slog.NewTextHandler(os.Stderr, nil))

// fakeStartProxy swaps the start/fail/stop signal seams and records which one
// fired, so the routing decision can be asserted without a live proxy.
type fakeStartProxy struct {
	started []int
	failed  []int
	stopped []int
}

func (f *fakeStartProxy) install(t *testing.T) {
	t.Helper()
	prevStart, prevFail, prevStop := proxyStart, proxyFail, proxyStop
	proxyStart = func(pid int) error {
		f.started = append(f.started, pid)
		return nil
	}
	proxyFail = func(pid int) error {
		f.failed = append(f.failed, pid)
		return nil
	}
	proxyStop = func(pid int) error {
		f.stopped = append(f.stopped, pid)
		return nil
	}
	t.Cleanup(func() {
		proxyStart = prevStart
		proxyFail = prevFail
		proxyStop = prevStop
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

// writeCreatedStateAnnotated persists a Created state carrying the
// annotations create would have enriched onto it. start reads them from the
// state, not from the bundle.
func writeCreatedStateAnnotated(t *testing.T, containerID, bundle string, annotations map[string]string) {
	t.Helper()
	state := oci.NewState(containerID, bundle)
	state.Status = specs.StateCreated
	state.Pid = 12345
	state.Annotations = annotations
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
	assert.Equal(t, []int{12345}, fake.stopped,
		"the proxy must not outlive the attempt that spawned it")

	state, err := oci.ReadState(containerID)
	require.NoError(t, err)
	assert.Equal(t, specs.StateCreated, state.Status, "a retryable failure must leave the container created")
}

// bundleWithRootfs writes an OCI bundle whose config points at an existing
// rootfs, so a test can lay out the extension's content first.
func bundleWithRootfs(t *testing.T, rootfs string, annotations map[string]string) string {
	t.Helper()
	bundle := t.TempDir()
	spec := specs.Spec{
		Version:     specs.Version,
		Root:        &specs.Root{Path: rootfs},
		Annotations: annotations,
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644))
	return bundle
}

// The extension's own hook runs before activation and short-circuits it. A
// hook that declined after the arm would leave Exited (1) beside an armed
// override, which no reader of either can reconcile.
func TestStart_DecliningHookRunsBeforeActivation(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "start-hook-first", "ext_test_abc_boot")

	var armed int
	armOverride = func(string) error {
		armed++
		return nil
	}

	annotations := map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}
	bundle := bundleWithRootfs(t, rootfs, annotations)
	hookDir := filepath.Join(rootfs, "hooks")
	require.NoError(t, os.MkdirAll(hookDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "start"),
		[]byte("#!/bin/sh\nexit 1\n"), 0o755))

	writeCreatedStateAnnotated(t, "start-hook-first", bundle, annotations)
	fake := &fakeStartProxy{}
	fake.install(t)

	require.NoError(t, Start(startTestLogger, "start-hook-first"))

	assert.Equal(t, []int{12345}, fake.failed, "the hook's verdict is the container's")
	assert.Zero(t, armed, "a declining hook must stop activation")
	_, err := os.Stat(bootByABIDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "a declining hook must leave no link")
}

// A declined activation is the extension's verdict and travels as the
// container's exit status, exactly as a declining hook does.
func TestStart_DeclinedActivationFailsTheContainerNotTheCall(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, _ := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "start-declined", "ext_test_abc_boot")

	annotations := map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: "0000000000000000000000000000000000000000000000000000000000000000",
	}
	bundle := bundleWithRootfs(t, rootfs, annotations)
	writeCreatedStateAnnotated(t, "start-declined", bundle, annotations)
	fake := &fakeStartProxy{}
	fake.install(t)

	require.NoError(t, Start(startTestLogger, "start-declined"))

	assert.Equal(t, []int{12345}, fake.failed)
	assert.Empty(t, fake.started)

	state, err := oci.ReadState("start-declined")
	require.NoError(t, err)
	assert.Equal(t, specs.StateStopped, state.Status)
}

// A machine condition fails the start call so the container stays created and
// the caller can deploy it again, rather than recording a permanent refusal.
func TestStart_RetryableActivationFailsTheCall(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "start-retryable", "ext_test_abc_boot")
	isMounted = func(string) (bool, error) { return false, nil }

	annotations := map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}
	bundle := bundleWithRootfs(t, rootfs, annotations)
	writeCreatedStateAnnotated(t, "start-retryable", bundle, annotations)
	fake := &fakeStartProxy{}
	fake.install(t)

	require.Error(t, Start(startTestLogger, "start-retryable"))

	assert.Empty(t, fake.failed, "a machine condition is not a verdict")
	assert.Empty(t, fake.started)
	assert.Equal(t, []int{12345}, fake.stopped, "the proxy must not be left behind")
}

func TestStart_ActivatedExtensionExitsZero(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := activateHost(t)
	rootfs, abi := activateRootfs(t, root, "kernel")
	fabricatedVolume(t, root, "start-armed", "ext_test_abc_boot")

	var armed []string
	armOverride = func(a string) error {
		armed = append(armed, a)
		return nil
	}

	annotations := map[string]string{
		labels.Class:       labels.ClassOverlay,
		labels.KernelABIID: abi,
	}
	bundle := bundleWithRootfs(t, rootfs, annotations)
	writeCreatedStateAnnotated(t, "start-armed", bundle, annotations)
	fake := &fakeStartProxy{}
	fake.install(t)

	require.NoError(t, Start(startTestLogger, "start-armed"))

	assert.Equal(t, []int{12345}, fake.started)
	assert.Equal(t, []string{abi}, armed)
}
