package manager

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/balena-os/balena-extension-runtime/internal/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const overlayClass = "io.balena.image.class"

func overlayLabels(extra map[string]string) map[string]string {
	out := map[string]string{overlayClass: "overlay"}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestCleanup_ZombieSweep_RemovesOnlyOnInspectError
// removes a listed container on exactly the inspects that report
// State.Error and on none of the clean ones. The stub alternates its answer,
// standing in for a Create that has not settled yet, so the same container
// reads failed on one sweep and clean on the next and the sweep has to
// re-decide every run rather than latch on its first answer.
func TestCleanup_ZombieSweep_RemovesOnlyOnInspectError(t *testing.T) {
	stub := newEngineStub()
	const id = "in-flight-cncr"
	stub.Containers = []Container{
		{ID: id, Image: "img1", State: "created", Labels: overlayLabels(nil)},
	}

	const sweeps = 50

	failed := inspectJSON(id, "created", "OCI runtime create failed: synthetic", 128)
	clean := inspectJSON(id, "created", "", 0)
	// The sweep issues exactly one inspect per listed container, so flipping
	// on every inspect alternates the answer Cleanup sees from run to run.
	reportFailed := false
	stub.onInspect = func(string) string {
		body := clean
		if reportFailed {
			body = failed
		}
		reportFailed = !reportFailed
		return body
	}

	testEngineEnv(t, testServer(t, stub.handler()))

	for i := 0; i < sweeps; i++ {
		err := Cleanup(context.Background(), quietLogger(), CleanupOpts{})
		assert.NoError(t, err)
	}

	// The stub keeps listing the container after each removal, so the sweep
	// re-decides every run: the removal count pins that it acted on exactly
	// the inspects reporting State.Error and on none of the clean ones.
	removed := stub.removedContainersSnapshot()
	assert.Len(t, removed, sweeps/2,
		"sweep must remove on exactly the inspects that report State.Error")
	for _, rid := range removed {
		assert.Equal(t, id, rid, "only the in-flight container ID is in scope")
	}
}

func TestStale(t *testing.T) {
	const runKver = "6.12.62"
	const runAbi = "abc123"
	const runOs = "2.119.0"

	cases := []struct {
		name   string
		labels map[string]string
		stale  bool
	}{
		{
			name:   "no labels (legacy) retained",
			labels: map[string]string{},
			stale:  false,
		},
		{
			name: "all three match",
			labels: map[string]string{
				"io.balena.image.kernel-abi-id":  runAbi,
				"io.balena.image.kernel-version": runKver,
				"io.balena.image.os-version":     "2.119.*",
			},
			stale: false,
		},
		{
			name: "abi mismatch",
			labels: map[string]string{
				"io.balena.image.kernel-abi-id":  "different",
				"io.balena.image.kernel-version": runKver,
				"io.balena.image.os-version":     "2.119.*",
			},
			stale: true,
		},
		{
			name: "kv mismatch (no abi claim)",
			labels: map[string]string{
				"io.balena.image.kernel-version": "6.12.61",
				"io.balena.image.os-version":     "2.119.*",
			},
			stale: true,
		},
		{
			name: "os mismatch (abi and kv match)",
			labels: map[string]string{
				"io.balena.image.kernel-abi-id":  runAbi,
				"io.balena.image.kernel-version": runKver,
				"io.balena.image.os-version":     "1.0.*",
			},
			stale: true,
		},
		{
			name: "kernel-agnostic, os match",
			labels: map[string]string{
				"io.balena.image.os-version": "2.*",
			},
			stale: false,
		},
		{
			name: "kernel-agnostic, os mismatch",
			labels: map[string]string{
				"io.balena.image.os-version": "1.*",
			},
			stale: true,
		},
		{
			name: "abi claim against empty running abi (absent balena_kernel_abi token, stock kernel)",
			labels: map[string]string{
				"io.balena.image.kernel-abi-id": "claim",
			},
			stale: true,
		},
	}
	logger := quietLogger()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stale(logger, tc.labels, runKver, runAbi, runOs)
			assert.Equal(t, tc.stale, got)
		})
	}

	t.Run("abi claim on device with no balena_kernel_abi token fails (runningAbi empty)", func(t *testing.T) {
		got := stale(
			logger,
			map[string]string{"io.balena.image.kernel-abi-id": "claim"},
			runKver, "", runOs,
		)
		assert.True(t, got, "extension claiming abi against a device with no published kernel-abi token is stale")
	})
}

func TestOsVersionMatch(t *testing.T) {
	const running = "2.119.0"
	cases := []struct {
		name  string
		label string
		want  bool
	}{
		{name: "empty label retains", label: "", want: true},
		{name: "whitespace-only label retains", label: "   ", want: true},
		{name: "exact match", label: "2.119.0", want: true},
		{name: "exact mismatch", label: "2.118.0", want: false},
		{name: "single glob match", label: "2.119.*", want: true},
		{name: "single glob mismatch", label: "2.118.*", want: false},
		{name: "broad glob match", label: "2.*", want: true},
		{name: "comma list first matches", label: "2.119.*,3.*", want: true},
		{name: "comma list second matches", label: "3.*,2.119.*", want: true},
		{name: "comma list no match", label: "3.*,4.*", want: false},
		{name: "whitespace around commas", label: " 2.119.* , 3.* ", want: true},
		{name: "trailing comma", label: "2.119.*,", want: true},
		{name: "only commas", label: ",,,", want: true},
	}
	logger := quietLogger()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, osVersionMatch(logger, tc.label, running))
		})
	}
}

func TestReadOSVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{
			name:    "unquoted",
			content: "ID=balena-os\nVERSION_ID=2.119.0\n",
			want:    "2.119.0",
		},
		{
			name:    "double-quoted with suffix",
			content: "VERSION_ID=\"2.119.0+rev1\"\n",
			want:    "2.119.0+rev1",
		},
		{
			name:    "single-quoted",
			content: "VERSION_ID='2.119.0'\n",
			want:    "2.119.0",
		},
		{
			name:    "ignores commented VERSION_ID",
			content: "# VERSION_ID=9.9.9\nVERSION_ID=2.119.0\n",
			want:    "2.119.0",
		},
		{
			name:    "missing",
			content: "ID=balena-os\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "os-release")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0644))
			got, err := readOSVersionFrom(path)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestOsVersionMatch_MalformedPatternLogged asserts that a typo in the
// os-version label is surfaced via logger.Warn instead of being silently
// retained — so a malformed pattern doesn't cause images to accumulate
// forever without any diagnostic trail.
func TestOsVersionMatch_MalformedPatternLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	got := osVersionMatch(logger, "2.119.[", "2.119.0")
	assert.True(t, got, "malformed pattern must retain rather than delete")
	out := buf.String()
	assert.Contains(t, out, "malformed os-version pattern")
	assert.Contains(t, out, "2.119.[")
}

func TestInspectContainer_ReturnsStateError(t *testing.T) {
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		if method == "GET" && path == "/containers/abc123/json" {
			return 200, []byte(`{"Id":"abc123","State":{"Status":"created","Error":"OCI runtime create failed: ...","ExitCode":128}}`)
		}
		return 404, nil
	})
	eng := testEngine(sock)
	got, err := eng.InspectContainer(context.Background(), "abc123")
	require.NoError(t, err)
	assert.Equal(t, "OCI runtime create failed: ...", got.State.Error)
	assert.Equal(t, 128, got.State.ExitCode)
	assert.Equal(t, "created", got.State.Status)
}

// TestCleanup_StaleOS_RetainsVolumeOfSurvivingContainer covers what a
// never-attached volume costs: it is dangling from birth, so the engine's
// in-use protection does not hold it back and the sweep is the only thing that
// can. Here the container's removal fails, so taking its volume would leave an
// armed override with nothing left to disarm it.
func TestCleanup_StaleOS_RetainsVolumeOfSurvivingContainer(t *testing.T) {
	const id = "aaaa000000000000"
	const imageID = "sha256:42befc76f4f8aaaa"

	stub := newEngineStub()
	stub.Containers = []Container{{
		ID:      id,
		ImageID: imageID,
		State:   "exited",
		Labels: overlayLabels(map[string]string{
			"io.balena.image.kernel-abi-id": "6.6.20-abc",
			"io.balena.image.os-version":    "9.9.*", // never matches running -> stale
			"io.balena.service-name":        "kernel-modules",
		}),
	}}
	stub.Inspects[id] = inspectJSON(id, "exited", "", 0)
	// The container removal fails, so the container is still on the device
	// when the volume sweep runs.
	stub.RemoveContainerStatus = map[string]int{id: 500}

	name := labels.VolumeName("kernel-modules", imageID)
	stub.Volumes = []Volume{{
		Name:   name,
		Labels: overlayLabels(map[string]string{"io.balena.image.os-version": "9.9.*"}),
	}}
	testEngineEnv(t, testServer(t, stub.handler()))

	osr := filepath.Join(t.TempDir(), "os-release")
	require.NoError(t, os.WriteFile(osr, []byte(`VERSION_ID="2.119.0"`+"\n"), 0o644))
	prev := osReleasePath
	osReleasePath = osr
	t.Cleanup(func() { osReleasePath = prev })

	err := Cleanup(context.Background(), quietLogger(), CleanupOpts{PruneStaleOS: true})
	require.Error(t, err, "the failed container removal must still be reported")

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.NotContains(t, stub.RemovedVolumes, name,
		"a volume must outlive the sweep while the container claiming it is still there")
}

// TestCleanup_StaleOS_CollectsVolumeOnceContainerIsGone is the other half:
// nothing claims the volume after its container is removed, so the sweep takes
// it. Without this the retention guard would simply leak every volume.
func TestCleanup_StaleOS_CollectsVolumeOnceContainerIsGone(t *testing.T) {
	const id = "aaaa000000000000"
	const imageID = "sha256:42befc76f4f8aaaa"

	stub := newEngineStub()
	stub.Containers = []Container{{
		ID:      id,
		ImageID: imageID,
		State:   "exited",
		Labels: overlayLabels(map[string]string{
			"io.balena.image.kernel-abi-id": "6.6.20-abc",
			"io.balena.image.os-version":    "9.9.*",
			"io.balena.service-name":        "kernel-modules",
		}),
	}}
	stub.Inspects[id] = inspectJSON(id, "exited", "", 0)

	name := labels.VolumeName("kernel-modules", imageID)
	stub.Volumes = []Volume{{
		Name:   name,
		Labels: overlayLabels(map[string]string{"io.balena.image.os-version": "9.9.*"}),
	}}
	testEngineEnv(t, testServer(t, stub.handler()))

	osr := filepath.Join(t.TempDir(), "os-release")
	require.NoError(t, os.WriteFile(osr, []byte(`VERSION_ID="2.119.0"`+"\n"), 0o644))
	prev := osReleasePath
	osReleasePath = osr
	t.Cleanup(func() { osReleasePath = prev })

	require.NoError(t, Cleanup(context.Background(), quietLogger(), CleanupOpts{PruneStaleOS: true}))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Contains(t, stub.RemovedContainers, id)
	assert.Contains(t, stub.RemovedVolumes, name)
}

// One walk serves both predicates, and a container the list already reports
// dead costs no inspect round trip.
func TestCleanup_CollectsDeadAndFailedCreateInOneWalk(t *testing.T) {
	const deadID = "dead000000000000"
	const zombieID = "zomb000000000000"
	const liveID = "live000000000000"

	stub := newEngineStub()
	stub.Containers = []Container{
		{ID: deadID, State: "dead", Labels: overlayLabels(nil)},
		{ID: zombieID, State: "created", Labels: overlayLabels(nil)},
		{ID: liveID, State: "exited", Labels: overlayLabels(nil)},
	}
	stub.Inspects[zombieID] = inspectJSON(zombieID, "created", "OCI runtime create failed: synthetic", 128)
	stub.Inspects[liveID] = inspectJSON(liveID, "exited", "", 0)
	testEngineEnv(t, testServer(t, stub.handler()))

	require.NoError(t, Cleanup(context.Background(), quietLogger(), CleanupOpts{}))

	assert.ElementsMatch(t, []string{deadID, zombieID}, stub.removedContainersSnapshot())
	assert.NotContains(t, stub.inspectedIDsSnapshot(), deadID,
		"a container the list already reports dead needs no inspect")
}

// Withdraw a kernel override on a device that stays on its OS and its volume is
// never stale, so a staleness predicate never takes it.
func TestCleanup_CollectsUnclaimedVolumeWithoutStaleOS(t *testing.T) {
	stub := newEngineStub()
	stub.Volumes = []Volume{
		{
			Name: "ext_kernel-modules_42befc76f4f8_boot",
			// Every claim it declares still holds against the running system.
			Labels: overlayLabels(map[string]string{"io.balena.image.os-version": "2.119.*"}),
		},
		{Name: "extra-firmware"},
	}
	testEngineEnv(t, testServer(t, stub.handler()))

	require.NoError(t, Cleanup(context.Background(), quietLogger(), CleanupOpts{}))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Equal(t, []string{"ext_kernel-modules_42befc76f4f8_boot"}, stub.RemovedVolumes,
		"a plain cleanup collects an extension volume no container claims")
	assert.NotContains(t, stub.RemovedVolumes, "extra-firmware",
		"a volume outside the extension class is not this sweep's to take")
}

// Pins the agreement between create and the sweep: both derive the same name
// from the same labels and image id.
func TestCleanup_RetainsVolumeClaimedByItsOwnContainer(t *testing.T) {
	const id = "aaaa000000000000"
	const imageID = "sha256:42befc76f4f8aaaa"

	stub := newEngineStub()
	stub.Containers = []Container{{
		ID:      id,
		ImageID: imageID,
		State:   "exited",
		Labels: overlayLabels(map[string]string{
			"io.balena.image.kernel-abi-id": "6.6.20-abc",
			"io.balena.service-name":        "kernel-modules",
		}),
	}}
	stub.Inspects[id] = inspectJSON(id, "exited", "", 0)

	name := labels.VolumeName("kernel-modules", imageID)
	stub.Volumes = []Volume{{Name: name, Labels: overlayLabels(nil)}}
	testEngineEnv(t, testServer(t, stub.handler()))

	require.NoError(t, Cleanup(context.Background(), quietLogger(), CleanupOpts{}))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Empty(t, stub.RemovedVolumes,
		"a volume its own container still claims must outlive the sweep")
}

// The volume sweep runs after every container pass, so a volume whose container
// this run removed is collected now rather than at the next boot.
func TestCleanup_CollectsVolumeFreedInTheSameSweep(t *testing.T) {
	const id = "dddd000000000000"
	const imageID = "sha256:42befc76f4f8aaaa"
	name := labels.VolumeName("kernel-modules", imageID)

	stub := newEngineStub()
	stub.Containers = []Container{{
		ID:      id,
		ImageID: imageID,
		State:   "dead",
		Labels: overlayLabels(map[string]string{
			"io.balena.image.kernel-abi-id": "6.6.20-abc",
			"io.balena.service-name":        "kernel-modules",
		}),
	}}
	stub.Volumes = []Volume{{Name: name, Labels: overlayLabels(nil)}}
	testEngineEnv(t, testServer(t, stub.handler()))

	require.NoError(t, Cleanup(context.Background(), quietLogger(), CleanupOpts{}))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Contains(t, stub.RemovedContainers, id)
	assert.Contains(t, stub.RemovedVolumes, name,
		"the volume sweep runs after the container passes, so it sees the removal")
}

// A deploy landing between the two snapshots must find its container in the
// claim set. The engine registers the container record before the runtime
// fabricates the volume, so the reverse order loses the volume just adopted.
func TestCleanup_VolumeSnapshotPrecedesTheClaimQuery(t *testing.T) {
	const id = "bbbb000000000000"
	const imageID = "sha256:42befc76f4f8aaaa"
	name := labels.VolumeName("kernel-modules", imageID)

	stub := newEngineStub()
	// A redeploy of the same service and image: the volume is already on disk.
	stub.Volumes = []Volume{{Name: name, Labels: overlayLabels(nil)}}
	stub.deployDuringVolumeList = func() []Container {
		return []Container{{
			ID:      id,
			ImageID: imageID,
			State:   "running",
			Labels: overlayLabels(map[string]string{
				"io.balena.image.kernel-abi-id": "6.6.20-abc",
				"io.balena.service-name":        "kernel-modules",
			}),
		}}
	}
	testEngineEnv(t, testServer(t, stub.handler()))

	require.NoError(t, Cleanup(context.Background(), quietLogger(), CleanupOpts{}))

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Empty(t, stub.RemovedVolumes,
		"a container registered before the claim query claims its volume")
}

// A claim set that cannot be completed is not an empty claim set, the rule
// extension-rollback's forget_unclaimed_abis states for the same predicate.
// The sweep takes nothing and the call fails.
func TestCleanup_UnanswerableClaimQueryAbandonsTheVolumeSweep(t *testing.T) {
	const id = "cccc000000000000"

	stub := newEngineStub()
	stub.Containers = []Container{{
		ID: id,
		// No ImageID, so this container's volume cannot be named.
		State: "running",
		Labels: overlayLabels(map[string]string{
			"io.balena.image.kernel-abi-id": "6.6.20-abc",
			"io.balena.service-name":        "kernel-modules",
		}),
	}}
	stub.Volumes = []Volume{{
		Name:   labels.VolumeName("kernel-modules", "sha256:42befc76f4f8aaaa"),
		Labels: overlayLabels(nil),
	}}
	testEngineEnv(t, testServer(t, stub.handler()))

	err := Cleanup(context.Background(), quietLogger(), CleanupOpts{})
	require.Error(t, err, "an unanswerable claim query must fail the unit")

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.Empty(t, stub.RemovedVolumes, "a sweep that cannot ask must not collect")
}
