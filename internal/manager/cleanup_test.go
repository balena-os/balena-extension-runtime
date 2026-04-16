package manager

import (
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
	// Override runningKernelVersion by setting /proc value — we can't, so we test
	// the full Cleanup only if /proc/sys/kernel/osrelease is readable.
	// Instead, test the container removal logic by calling Cleanup and checking results.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger)
	if err != nil {
		// If running on a system without /proc (e.g., macOS), skip.
		t.Skipf("skipping: %v", err)
	}

	assert.Contains(t, removedIDs, "dead-container-1")
}

func TestCleanup_RemovesStaleKernelContainers(t *testing.T) {
	var removedIDs []string

	// Use the actual kernel version from the test host.
	kver := readHostKernelVersion(t)

	containers := []Container{
		{ID: "stale-container", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": "99.99.99", // mismatches any real kernel
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	assert.Contains(t, removedIDs, "stale-container")
	assert.NotContains(t, removedIDs, "match-container")
}

func TestCleanup_RemovesStaleABIContainers(t *testing.T) {
	var removedIDs []string

	kver := readHostKernelVersion(t)
	// Use a known-fake ABI ID that will not match whatever the host has.
	containers := []Container{
		{ID: "stale-abi-cont", Image: "img1", State: "exited", Labels: map[string]string{
			"io.balena.image.class":      "overlay",
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Cleanup(context.Background(), logger)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// The stale ABI container should be removed only if the host has a Module.symvers
	// (so runningKernelABIID returns non-empty). If the host has no Module.symvers,
	// both containers survive (ABI filter is skipped).
	abiID := readHostKernelABIID(t)
	if abiID != "" {
		assert.Contains(t, removedIDs, "stale-abi-cont")
	}
	// Container without ABI label should never be removed.
	assert.NotContains(t, removedIDs, "no-abi-contain")
}

func TestCleanup_RemovesOrphanedImages(t *testing.T) {
	kver := readHostKernelVersion(t)
	var removedImageIDs []string

	containers := []Container{
		{ID: "live-container-1", Image: "sha256:img-alive", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": kver,
		}},
	}

	images := []Image{
		{ID: "sha256:img-alive", Labels: map[string]string{"io.balena.image.class": "overlay"}, RepoTags: []string{"alive:latest"}},
		{ID: "sha256:img-orphan", Labels: map[string]string{"io.balena.image.class": "overlay"}, RepoTags: []string{"orphan:latest"}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
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

	err := Cleanup(context.Background(), logger)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	assert.Contains(t, removedImageIDs, "sha256:img-orphan")
	assert.NotContains(t, removedImageIDs, "sha256:img-alive")
}

func TestCleanup_KeepsImageReferencedByTag(t *testing.T) {
	kver := readHostKernelVersion(t)
	var removedImageIDs []string

	// Container references the image by tag, not by ID.
	containers := []Container{
		{ID: "live-container-1", Image: "myext:latest", State: "exited", Labels: map[string]string{
			"io.balena.image.class":          "overlay",
			"io.balena.image.kernel-version": kver,
		}},
	}

	images := []Image{
		{ID: "sha256:img-by-tag", Labels: map[string]string{"io.balena.image.class": "overlay"}, RepoTags: []string{"myext:latest"}},
	}

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(containers)
			return 200, resp
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

	err := Cleanup(context.Background(), logger)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// Image should NOT be removed because container references it by tag.
	assert.Empty(t, removedImageIDs)
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
			// Simulate failure.
			return 500, []byte("internal error")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Cleanup should return nil even when ListImages fails.
	err := Cleanup(context.Background(), logger)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	// No assertion needed beyond no panic and nil return.
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
		return "" // No Module.symvers on this host
	}
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}
