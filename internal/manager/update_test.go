package manager

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadKernelVersion_Simple(t *testing.T) {
	rootfs := withModulesDir(t, "6.6.74")
	ver, err := readKernelVersion(rootfs)
	require.NoError(t, err)
	assert.Equal(t, "6.6.74", ver)
}

func TestReadKernelVersion_WithSuffix(t *testing.T) {
	rootfs := withModulesDir(t, "6.6.74-v8")
	ver, err := readKernelVersion(rootfs)
	require.NoError(t, err)
	assert.Equal(t, "6.6.74", ver)
}

func TestReadKernelVersion_SkipsNonNumeric(t *testing.T) {
	rootfs := withModulesDir(t, "source", "README", "5.15.0")
	ver, err := readKernelVersion(rootfs)
	require.NoError(t, err)
	assert.Equal(t, "5.15.0", ver)
}

func TestReadKernelVersion_OnlyNonNumeric(t *testing.T) {
	rootfs := withModulesDir(t, "source", "README")
	_, err := readKernelVersion(rootfs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no kernel version found")
}

func TestReadKernelVersion_EmptyDir(t *testing.T) {
	rootfs := withModulesDir(t)
	_, err := readKernelVersion(rootfs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no kernel version found")
}

func TestReadKernelVersion_MissingDir(t *testing.T) {
	rootfs := t.TempDir() // no lib/modules at all
	_, err := readKernelVersion(rootfs)
	require.Error(t, err)
}

func TestReadKernelVersion_SkipsFiles(t *testing.T) {
	rootfs := t.TempDir()
	modulesDir := filepath.Join(rootfs, "lib", "modules")
	require.NoError(t, os.MkdirAll(modulesDir, 0o755))
	// Create a file (not directory) with a numeric name.
	require.NoError(t, os.WriteFile(filepath.Join(modulesDir, "6.6.74"), nil, 0o644))
	// Create a valid directory.
	require.NoError(t, os.Mkdir(filepath.Join(modulesDir, "5.10.0"), 0o755))

	ver, err := readKernelVersion(rootfs)
	require.NoError(t, err)
	assert.Equal(t, "5.10.0", ver)
}

func TestUpdate(t *testing.T) {
	// Prepare a rootfs with a kernel version.
	rootfs := withModulesDir(t, "6.6.74")

	// Track API calls.
	var createdImages []string
	var startedIDs []string

	containers := []Container{
		{ID: "c1-existing", Image: "ext-a:latest", State: "exited", Labels: map[string]string{"io.balena.image.class": "overlay"}},
		{ID: "c2-existing", Image: "ext-b:latest", State: "exited", Labels: map[string]string{"io.balena.image.class": "overlay"}},
	}

	sock := testServer(t, func(method, path string, body []byte) (int, []byte) {
		switch {
		case method == "GET" && path[:len("/containers/json")] == "/containers/json":
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "POST" && path == "/containers/create":
			var req map[string]any
			json.Unmarshal(body, &req)
			createdImages = append(createdImages, req["Image"].(string))
			// Verify kernel-version label is set.
			lbls := req["Labels"].(map[string]any)
			assert.Equal(t, "6.6.74", lbls["io.balena.image.kernel-version"])
			return 201, []byte(`{"Id":"new-` + req["Image"].(string) + `"}`)
		case method == "POST":
			// StartContainer
			startedIDs = append(startedIDs, path)
			return 204, nil
		default:
			return 404, []byte("not found")
		}
	})

	testEngineEnv(t, sock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Update(context.Background(), logger, rootfs)
	require.NoError(t, err)

	assert.Equal(t, []string{"ext-a:latest", "ext-b:latest"}, createdImages)
	require.Len(t, startedIDs, 2)
}

func TestUpdate_CreateFailure_Continues(t *testing.T) {
	rootfs := withModulesDir(t, "6.6.74")
	var startCount int

	containers := []Container{
		{ID: "c1", Image: "ext-fail:latest", State: "exited", Labels: map[string]string{"io.balena.image.class": "overlay"}},
		{ID: "c2", Image: "ext-ok:latest", State: "exited", Labels: map[string]string{"io.balena.image.class": "overlay"}},
	}

	sock := testServer(t, func(method, path string, body []byte) (int, []byte) {
		switch {
		case method == "GET" && path[:len("/containers/json")] == "/containers/json":
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "POST" && path == "/containers/create":
			var req map[string]any
			json.Unmarshal(body, &req)
			if req["Image"] == "ext-fail:latest" {
				return 500, []byte("create failed")
			}
			return 201, []byte(`{"Id":"new-ok"}`)
		case method == "POST":
			startCount++
			return 204, nil
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	err := Update(context.Background(), logger, rootfs)
	require.NoError(t, err)
	// Only the second container should have been started.
	assert.Equal(t, 1, startCount)
}

func TestUpdate_StartFailure_Continues(t *testing.T) {
	rootfs := withModulesDir(t, "6.6.74")

	containers := []Container{
		{ID: "c1", Image: "ext-a:latest", State: "exited", Labels: map[string]string{"io.balena.image.class": "overlay"}},
	}

	sock := testServer(t, func(method, path string, body []byte) (int, []byte) {
		switch {
		case method == "GET" && path[:len("/containers/json")] == "/containers/json":
			resp, _ := json.Marshal(containers)
			return 200, resp
		case method == "POST" && path == "/containers/create":
			return 201, []byte(`{"Id":"new-c1"}`)
		case method == "POST":
			// Start fails.
			return 500, []byte("start failed")
		default:
			return 404, nil
		}
	})

	testEngineEnv(t, sock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Update should not return an error when start fails (it logs a warning).
	err := Update(context.Background(), logger, rootfs)
	require.NoError(t, err)
}
