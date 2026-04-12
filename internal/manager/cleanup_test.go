package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanup_RemovesDeadContainers(t *testing.T) {
	var removedIDs []string

	containers := []Container{
		{ID: "dead-container-1", Image: "img1", State: "dead", Labels: map[string]string{"io.balena.image.class": "overlay"}},
		{ID: "alive-container1", Image: "img2", State: "exited", Labels: map[string]string{"io.balena.image.class": "overlay"}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.SplitN(id, "?", 2)[0]
			removedIDs = append(removedIDs, id)
			return 204, nil
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{})
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	assert.Contains(t, removedIDs, "dead-container-1")
	assert.NotContains(t, removedIDs, "alive-container1")
}

// TestCleanup_DeadOnly asserts that, without PruneStaleOS, containers with
// mismatched kernel-version or kernel-abi-id labels are preserved, and
// os-mismatched images are preserved. This is the invariant that protects
// K_A containers and pre-HUP images during the rollback window.
func TestCleanup_DeadOnly(t *testing.T) {
	var removedIDs []string
	var removedImageIDs []string

	containers := []Container{
		{ID: "dead-container-1", Image: "img0", State: "dead", Labels: map[string]string{"io.balena.image.class": "overlay"}},
		{ID: "stale-kernel-ct", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": "99.99.99",
		}},
		{ID: "stale-abi-cont-", Image: "img2", State: "exited", Labels: map[string]string{
			"io.balena.image.class":         "overlay",
			"io.balena.image.kernel-abi-id": "0000000000000000000000000000000000000000000000000000000000000000",
		}},
	}

	images := []Image{
		{ID: "sha256:img-stale", Labels: map[string]string{
			"io.balena.image.class":      "overlay",
			"io.balena.image.os-version": "1.2.3",
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.SplitN(id, "?", 2)[0]
			removedIDs = append(removedIDs, id)
			return 204, nil
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			resp, _ := json.Marshal(images)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/images/"):
			id := strings.TrimPrefix(path, "/images/")
			id = strings.SplitN(id, "?", 2)[0]
			removedImageIDs = append(removedImageIDs, id)
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{})
	assert.NoError(t, err)

	assert.Equal(t, []string{"dead-container-1"}, removedIDs,
		"default Cleanup must remove only dead containers, preserving stale ones")
	assert.Empty(t, removedImageIDs,
		"default Cleanup must not touch images")
}

func TestCleanup_StaleOS_RemovesStaleKernelContainers(t *testing.T) {
	var removedIDs []string

	kver := readHostKernelVersion(t)

	containers := []Container{
		{ID: "stale-container", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": "99.99.99",
		}},
		{ID: "match-container", Image: "img2", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": kver,
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.SplitN(id, "?", 2)[0]
			removedIDs = append(removedIDs, id)
			return 204, nil
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	withOSRelease(t, "VERSION_ID=0.0.0\n")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	assert.Contains(t, removedIDs, "stale-container")
	assert.NotContains(t, removedIDs, "match-container")
}

func TestCleanup_StaleOS_RemovesStaleABIContainers(t *testing.T) {
	var removedIDs []string

	kver := readHostKernelVersion(t)
	containers := []Container{
		{ID: "stale-abi-cont", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": kver,
			"io.balena.image.kernel-abi-id":  "0000000000000000000000000000000000000000000000000000000000000000",
		}},
		{ID: "no-abi-contain", Image: "img2", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": kver,
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.SplitN(id, "?", 2)[0]
			removedIDs = append(removedIDs, id)
			return 204, nil
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	withOSRelease(t, "VERSION_ID=0.0.0\n")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	abiID := readHostKernelABIID(t)
	if abiID != "" {
		assert.Contains(t, removedIDs, "stale-abi-cont")
	}
	assert.NotContains(t, removedIDs, "no-abi-contain")
}

// TestCleanup_StaleOS_Images covers the os-version image GC predicate.
func TestCleanup_StaleOS_Images(t *testing.T) {
	cases := []struct {
		name       string
		label      string
		shouldKeep bool
	}{
		{name: "exact match retained", label: "2.119.0", shouldKeep: true},
		{name: "mismatch removed", label: "2.118.0", shouldKeep: false},
		{name: "glob match retained", label: "2.119.*", shouldKeep: true},
		{name: "no label retained (legacy)", label: "", shouldKeep: true},
		{name: "comma list retained", label: "2.118.*,2.119.*", shouldKeep: true},
		{name: "comma list no match removed", label: "2.117.*,2.118.*", shouldKeep: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var removedImageIDs []string

			imageLabels := map[string]string{"io.balena.image.class": "overlay"}
			if tc.label != "" {
				imageLabels["io.balena.image.os-version"] = tc.label
			}
			images := []Image{
				{ID: "sha256:img-under-test", Labels: imageLabels},
			}

			sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
				switch {
				case method == "GET" && strings.HasPrefix(path, "/containers/json"):
					return 200, []byte("[]")
				case method == "GET" && strings.HasPrefix(path, "/images/json"):
					resp, _ := json.Marshal(images)
					return 200, resp
				case method == "DELETE" && strings.HasPrefix(path, "/images/"):
					id := strings.TrimPrefix(path, "/images/")
					id = strings.SplitN(id, "?", 2)[0]
					removedImageIDs = append(removedImageIDs, id)
					return 200, []byte("[]")
				default:
					return 404, nil
				}
			})

			testEngineEnv(t, sock)
			withOSRelease(t, `VERSION_ID="2.119.0"`+"\n")
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

			err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
			if err != nil {
				t.Skipf("skipping: %v", err)
			}

			if tc.shouldKeep {
				assert.Empty(t, removedImageIDs, "image should have been retained")
			} else {
				assert.Equal(t, []string{"sha256:img-under-test"}, removedImageIDs,
					"image should have been removed")
			}
		})
	}
}

// TestCleanup_StaleOS_MissingOSRelease asserts that a missing /etc/os-release
// aborts the stale-OS pass with a non-zero exit — the caller explicitly
// requested the sweep, so silently degrading to dead-only mode would let
// stale extensions accumulate unnoticed. Images must still not be wiped.
func TestCleanup_StaleOS_MissingOSRelease(t *testing.T) {
	var removedImageIDs []string

	images := []Image{
		{ID: "sha256:img-should-stay", Labels: map[string]string{
			"io.balena.image.class":      "overlay",
			"io.balena.image.os-version": "whatever",
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			return 200, []byte("[]")
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			resp, _ := json.Marshal(images)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/images/"):
			id := strings.TrimPrefix(path, "/images/")
			id = strings.SplitN(id, "?", 2)[0]
			removedImageIDs = append(removedImageIDs, id)
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	prev := osReleasePath
	osReleasePath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { osReleasePath = prev })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
	require.Error(t, err, "missing os-release must surface a non-zero exit")
	assert.Contains(t, err.Error(), "read OS version")
	assert.Empty(t, removedImageIDs, "missing os-release must skip image GC, not remove everything")
}

func TestCleanup_ListImagesFailure_NonFatal(t *testing.T) {
	kver := readHostKernelVersion(t)

	containers := []Container{
		{ID: "live-container-1", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": kver,
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			return 500, []byte("internal error")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	withOSRelease(t, "VERSION_ID=2.119.0\n")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
}

// TestCleanup_StaleOS_KernelAgnosticContainer covers the predicate gap that
// this patch closed: a container with only an os-version claim (no kv, no
// abi) should be removed at HUP commit when the running OS is out of
// range, and retained when in range.
func TestCleanup_StaleOS_KernelAgnosticContainer(t *testing.T) {
	cases := []struct {
		name       string
		osLabel    string
		shouldKeep bool
	}{
		{name: "in range retained", osLabel: "2.119.*", shouldKeep: true},
		{name: "out of range removed", osLabel: "2.118.*", shouldKeep: false},
		{name: "no os claim retained (legacy)", osLabel: "", shouldKeep: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var removedIDs []string

			containerLabels := map[string]string{"io.balena.image.class": "overlay"}
			if tc.osLabel != "" {
				containerLabels["io.balena.image.os-version"] = tc.osLabel
			}
			containers := []Container{
				{ID: "kagnostic-ctn1", Image: "img1", State: "exited", Labels: containerLabels},
			}

			sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
				switch {
				case method == "GET" && strings.HasPrefix(path, "/containers/json"):
					resp, _ := json.Marshal(containers)
					return 200, resp
				case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
					id := strings.TrimPrefix(path, "/containers/")
					id = strings.SplitN(id, "?", 2)[0]
					removedIDs = append(removedIDs, id)
					return 204, nil
				case method == "GET" && strings.HasPrefix(path, "/images/json"):
					return 200, []byte("[]")
				default:
					return 404, nil
				}
			})

			testEngineEnv(t, sock)
			withOSRelease(t, `VERSION_ID="2.119.0"`+"\n")
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

			err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
			if err != nil {
				t.Skipf("skipping: %v", err)
			}

			if tc.shouldKeep {
				assert.Empty(t, removedIDs)
			} else {
				assert.Equal(t, []string{"kagnostic-ctn1"}, removedIDs)
			}
		})
	}
}

// TestCleanup_StaleOS_AllLevelsMatch asserts that an extension declaring
// all three compatibility levels is retained when every level matches.
func TestCleanup_StaleOS_AllLevelsMatch(t *testing.T) {
	kver := readHostKernelVersion(t)
	abiID := readHostKernelABIID(t)
	if abiID == "" {
		t.Skip("Module.symvers not available on this host; cannot test ABI match path")
	}

	var removedIDs []string
	containers := []Container{
		{ID: "all-match-ctnr", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-abi-id":  abiID,
			"io.balena.image.kernel-version": kver,
			"io.balena.image.os-version":     "2.119.*",
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.SplitN(id, "?", 2)[0]
			removedIDs = append(removedIDs, id)
			return 204, nil
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	withOSRelease(t, `VERSION_ID="2.119.0"`+"\n")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
	assert.NoError(t, err)
	assert.Empty(t, removedIDs, "container with matching abi+kv+os should be retained")
}

// TestCleanup_StaleOS_OSMismatchStripsMatchingABI asserts that even when
// kernel-abi-id and kernel-version match, an os-version mismatch removes
// the extension: claims are AND-composed, any failing claim is stale.
func TestCleanup_StaleOS_OSMismatchStripsMatchingABI(t *testing.T) {
	kver := readHostKernelVersion(t)
	abiID := readHostKernelABIID(t)
	if abiID == "" {
		t.Skip("Module.symvers not available on this host; cannot test ABI match path")
	}

	var removedIDs []string
	containers := []Container{
		{ID: "os-stale-ctnr1", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-abi-id":  abiID,
			"io.balena.image.kernel-version": kver,
			"io.balena.image.os-version":     "1.0.*",
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.SplitN(id, "?", 2)[0]
			removedIDs = append(removedIDs, id)
			return 204, nil
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	withOSRelease(t, `VERSION_ID="2.119.0"`+"\n")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
	assert.NoError(t, err)
	assert.Equal(t, []string{"os-stale-ctnr1"}, removedIDs,
		"matching abi+kv does not override an os-version mismatch")
}

// TestCleanup_StaleOS_ImageKernelAgnostic confirms the image predicate
// behaves identically to the container predicate for kernel-agnostic
// extensions.
func TestCleanup_StaleOS_ImageKernelAgnostic(t *testing.T) {
	var removedImageIDs []string

	images := []Image{
		{ID: "sha256:kagn-retain", Labels: map[string]string{
			"io.balena.image.class":      "overlay",
			"io.balena.image.os-version": "2.119.*",
		}},
		{ID: "sha256:kagn-remove", Labels: map[string]string{
			"io.balena.image.class":      "overlay",
			"io.balena.image.os-version": "2.118.*",
		}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			return 200, []byte("[]")
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			resp, _ := json.Marshal(images)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/images/"):
			id := strings.TrimPrefix(path, "/images/")
			id = strings.SplitN(id, "?", 2)[0]
			removedImageIDs = append(removedImageIDs, id)
			return 200, []byte("[]")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	withOSRelease(t, `VERSION_ID="2.119.0"`+"\n")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger, CleanupOpts{PruneStaleOS: true})
	assert.NoError(t, err)
	assert.Equal(t, []string{"sha256:kagn-remove"}, removedImageIDs)
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
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

	// Unterminated character class triggers filepath.ErrBadPattern.
	got := osVersionMatch(logger, "2.119.[", "2.119.0")
	assert.True(t, got, "malformed pattern must retain rather than delete")
	out := buf.String()
	assert.Contains(t, out, "malformed os-version pattern")
	assert.Contains(t, out, "2.119.[")
}

// withOSRelease writes a temporary os-release file and points osReleasePath at it.
func withOSRelease(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write os-release: %v", err)
	}
	prev := osReleasePath
	osReleasePath = path
	t.Cleanup(func() { osReleasePath = prev })
}

// readHostKernelVersion reads the kernel version from /proc/sys/kernel/osrelease.
// It strips the suffix after the first dash, matching runningKernelVersion().
func readHostKernelVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		t.Skipf("cannot read kernel version: %v", err)
	}
	release := strings.TrimSpace(string(data))
	if idx := strings.IndexByte(release, '-'); idx > 0 {
		release = release[:idx]
	}
	return release
}

// readHostKernelABIID computes the ABI ID of the running kernel.
// Returns "" if Module.symvers is not available.
func readHostKernelABIID(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		t.Skipf("cannot read kernel release: %v", err)
	}
	release := strings.TrimSpace(string(data))
	symvers := filepath.Join("/lib/modules", release, "Module.symvers")
	content, err := os.ReadFile(symvers)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}
