package manager

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

// TestCleanup_ZombieSweep_ConcurrentInFlightCreate exercises a real
// concurrent scenario that the integration suite cannot reach. A
// background goroutine repeatedly flips the engine's reported
// State.Error between empty and populated while Cleanup runs in
// parallel — modelling a Create that may transition to failed at any
// moment relative to Cleanup's inspect call.
//
// The test pins two invariants: (1) no data races on shared engine
// state under `go test -race`, (2) the only ID that ever ends up in
// the removed set is the in-flight container ID. Anything else would
// indicate Cleanup is making removal decisions from stale state.
func TestCleanup_ZombieSweep_ConcurrentInFlightCreate(t *testing.T) {
	stub := newEngineStub()
	const id = "in-flight-cncr"
	stub.Containers = []Container{
		{ID: id, Image: "img1", State: "created", Labels: overlayLabels(nil)},
	}
	stub.Inspects[id] = inspectJSON(id, "created", "", 0)

	testEngineEnv(t, testServer(t, stub.handler()))

	stop := make(chan struct{})
	done := make(chan struct{})
	var flips atomic.Int64

	go func() {
		defer close(done)
		failed := inspectJSON(id, "created", "OCI runtime create failed: synthetic", 128)
		clean := inspectJSON(id, "created", "", 0)
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			stub.mu.Lock()
			if toggle {
				stub.Inspects[id] = failed
			} else {
				stub.Inspects[id] = clean
			}
			stub.mu.Unlock()
			toggle = !toggle
			flips.Add(1)
			time.Sleep(50 * time.Microsecond)
		}
	}()

	for i := 0; i < 50; i++ {
		err := Cleanup(context.Background(), quietLogger(), CleanupOpts{})
		assert.NoError(t, err)
	}
	close(stop)
	<-done

	assert.Greater(t, flips.Load(), int64(50),
		"flipper must run alongside Cleanup; bump cycles if this fails")

	for _, rid := range stub.removedContainersSnapshot() {
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
			name: "abi claim against empty running abi (missing Module.symvers)",
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

	t.Run("abi claim on device with no symvers fails (runningAbi empty)", func(t *testing.T) {
		got := stale(
			logger,
			map[string]string{"io.balena.image.kernel-abi-id": "claim"},
			runKver, "", runOs,
		)
		assert.True(t, got, "extension claiming abi against symvers-less device is stale")
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
