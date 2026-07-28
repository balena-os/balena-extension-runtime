package manager

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

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

func TestParseKernelABIID(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"present", "console=tty1 balena_kernel_abi=0123abcd rootwait", "0123abcd"},
		{"absent (stock kernel)", "console=tty1 rootwait", ""},
		{"empty value", "balena_kernel_abi= rootwait", ""},
		{"prefix of another token does not match", "not_balena_kernel_abi=x", ""},
		// Real /proc/cmdline ends in a newline and often carries the token
		// as the final field; Fields must swallow the trailing \n.
		{"token last, trailing newline", "console=tty1 balena_kernel_abi=0123abcd\n", "0123abcd"},
		// First match wins, matching mobynit's parser: a later duplicate
		// must not override the initrd's first published value.
		{"duplicate token keeps the first", "balena_kernel_abi=first balena_kernel_abi=second", "first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseKernelABIID(tt.cmdline); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
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

// TestCleanup_StaleOS_RemovesStaleExtensionVolumes asserts the volume sweep
// is ownership+staleness by label.
func TestCleanup_StaleOS_RemovesStaleExtensionVolumes(t *testing.T) {
	stub := newEngineStub()
	stub.Volumes = []Volume{
		{
			Name:   "ext_kernel-modules_42befc76f4f8_boot",
			Labels: overlayLabels(map[string]string{"io.balena.image.os-version": "2.118.*"}),
		},
		{
			Name:   "ext_other_aabbccddeeff_lib_modules",
			Labels: overlayLabels(map[string]string{"io.balena.image.os-version": "2.119.*"}),
		},
		{Name: "extra-firmware"},
	}
	sock := testServer(t, stub.handler())
	testEngineEnv(t, sock)

	osr := filepath.Join(t.TempDir(), "os-release")
	require.NoError(t, os.WriteFile(osr, []byte(`VERSION_ID="2.119.0"`+"\n"), 0o644))
	prev := osReleasePath
	osReleasePath = osr
	t.Cleanup(func() { osReleasePath = prev })

	err := Cleanup(context.Background(), quietLogger(), CleanupOpts{PruneStaleOS: true})
	require.NoError(t, err)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	assert.ElementsMatch(t, []string{
		"ext_kernel-modules_42befc76f4f8_boot",
	}, stub.RemovedVolumes)
	assert.NotContains(t, stub.RemovedVolumes, "ext_other_aabbccddeeff_lib_modules")
	assert.NotContains(t, stub.RemovedVolumes, "extra-firmware")
}
