package manager

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDo_GET_200(t *testing.T) {
	body := `{"status":"ok"}`
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "GET", method)
		assert.Equal(t, "/test", path)
		return 200, []byte(body)
	})

	eng := testEngine(sock)
	resp, err := eng.do(context.Background(), "GET", "/test", nil)
	require.NoError(t, err)
	assert.JSONEq(t, body, string(resp))
}

func TestDo_POST_201(t *testing.T) {
	reqBody := `{"key":"value"}`
	sock := testServer(t, func(method, path string, body []byte) (int, []byte) {
		assert.Equal(t, "POST", method)
		assert.JSONEq(t, reqBody, string(body))
		return 201, []byte(`{"Id":"abc123"}`)
	})

	eng := testEngine(sock)
	resp, err := eng.do(context.Background(), "POST", "/create", []byte(reqBody))
	require.NoError(t, err)
	assert.Contains(t, string(resp), "abc123")
}

func TestDo_DELETE_204(t *testing.T) {
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "DELETE", method)
		return 204, nil
	})

	eng := testEngine(sock)
	resp, err := eng.do(context.Background(), "DELETE", "/containers/abc?force=true", nil)
	require.NoError(t, err)
	assert.Empty(t, resp)
}

func TestDo_404_Error(t *testing.T) {
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		return 404, []byte("no such container")
	})

	eng := testEngine(sock)
	_, err := eng.do(context.Background(), "GET", "/missing", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
	assert.Contains(t, err.Error(), "no such container")
}

func TestDo_ConnectionRefused(t *testing.T) {
	eng := testEngine("/tmp/nonexistent-socket-" + t.Name() + ".sock")
	_, err := eng.do(context.Background(), "GET", "/test", nil)
	require.Error(t, err)
}

func TestListContainers(t *testing.T) {
	containers := []Container{
		{ID: "c1", Image: "img1", State: "running", Labels: map[string]string{"io.balena.image.class": "overlay"}},
		{ID: "c2", Image: "img2", State: "exited", Labels: map[string]string{"io.balena.image.class": "overlay"}},
	}
	respBody, _ := json.Marshal(containers)

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "GET", method)
		assert.Contains(t, path, "/containers/json")
		assert.Contains(t, path, "all=true")
		// Verify filters are URL-encoded.
		assert.Contains(t, path, url.QueryEscape(`{"label":["io.balena.image.class=overlay"]}`))
		return 200, respBody
	})

	eng := testEngine(sock)
	result, err := eng.ListContainers(context.Background(), "io.balena.image.class=overlay")
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, "c1", result[0].ID)
	assert.Equal(t, "c2", result[1].ID)
}

func TestRemoveContainer(t *testing.T) {
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "DELETE", method)
		assert.True(t, strings.HasPrefix(path, "/containers/abc123"))
		assert.Contains(t, path, "force=true")
		assert.Contains(t, path, "v=true")
		return 204, nil
	})

	eng := testEngine(sock)
	require.NoError(t, eng.RemoveContainer(context.Background(), "abc123"))
}

func TestListImages(t *testing.T) {
	images := []Image{
		{ID: "sha256:img1", Labels: map[string]string{"io.balena.image.class": "overlay"}, RepoTags: []string{"myimg:latest"}},
	}
	respBody, _ := json.Marshal(images)

	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "GET", method)
		assert.Contains(t, path, "/images/json")
		return 200, respBody
	})

	eng := testEngine(sock)
	result, err := eng.ListImages(context.Background(), "io.balena.image.class=overlay")
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "sha256:img1", result[0].ID)
	assert.Equal(t, []string{"myimg:latest"}, result[0].RepoTags)
}

func TestRemoveImage(t *testing.T) {
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "DELETE", method)
		assert.Contains(t, path, "/images/sha256:img1")
		assert.Contains(t, path, "force=true")
		return 200, []byte(`[]`)
	})

	eng := testEngine(sock)
	require.NoError(t, eng.RemoveImage(context.Background(), "sha256:img1"))
}

func TestNewEngine_Default(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	eng := NewEngine()
	assert.Equal(t, defaultSocket, eng.socket)
}

func TestNewEngine_CustomSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///tmp/custom.sock")
	eng := NewEngine()
	assert.Equal(t, "/tmp/custom.sock", eng.socket)
}

// blackholeSocket accepts connections but never replies. Used to exercise
// deadline and cancellation paths in do().
func blackholeSocket(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "bh.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				<-done
				_ = conn.Close()
			}(c)
		}
	}()
	return sock
}

// TestDo_DeadlineFromContext verifies the conn deadline honours ctx.Deadline
// when the caller has set one shorter than the 30s ceiling.
func TestDo_DeadlineFromContext(t *testing.T) {
	sock := blackholeSocket(t)
	eng := testEngine(sock)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := eng.do(ctx, "GET", "/test", nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second, "should return well before defaultTimeout")
}

// TestDo_ContextCancelUnblocksRead verifies cancelling the context while
// a read is in flight closes the conn and unblocks the read promptly.
func TestDo_ContextCancelUnblocksRead(t *testing.T) {
	sock := blackholeSocket(t)
	eng := testEngine(sock)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := eng.do(ctx, "GET", "/test", nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 1*time.Second, "ctx cancel should unblock read")
}

// TestDo_ResponseSizeCap verifies responses exceeding maxResponseBytes are
// rejected rather than read into memory indefinitely.
func TestDo_ResponseSizeCap(t *testing.T) {
	defer func(saved int) { maxResponseBytes = saved }(maxResponseBytes)
	maxResponseBytes = 100

	big := strings.Repeat("A", 200)
	sock := testServer(t, func(_, _ string, _ []byte) (int, []byte) {
		return 200, []byte(big)
	})

	eng := testEngine(sock)
	_, err := eng.do(context.Background(), "GET", "/test", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestCheckSocket_Valid(t *testing.T) {
	sock := testServer(t, func(_, _ string, _ []byte) (int, []byte) { return 200, nil })
	eng := testEngine(sock)
	assert.NoError(t, eng.CheckSocket())
}

func TestCheckSocket_Missing(t *testing.T) {
	eng := testEngine(filepath.Join(t.TempDir(), "absent.sock"))
	err := eng.CheckSocket()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "DOCKER_HOST", "error should mention override hint")
}

func TestCheckSocket_NotASocket(t *testing.T) {
	regularFile := filepath.Join(t.TempDir(), "regular-file")
	require.NoError(t, os.WriteFile(regularFile, []byte("not a socket"), 0o600))

	eng := testEngine(regularFile)
	err := eng.CheckSocket()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a unix socket")
}

func TestListVolumes_Dangling(t *testing.T) {
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "GET", method)
		assert.True(t, strings.HasPrefix(path, "/volumes"))
		expectedFilter := "filters=" + url.QueryEscape(`{"dangling":["true"]}`)
		assert.Contains(t, path, expectedFilter)
		return 200, []byte(`{"Volumes":[{"Name":"v1","Labels":{"io.balena.image.class":"overlay"}},{"Name":"v2","Labels":null}]}`)
	})
	eng := testEngine(sock)
	vols, err := eng.ListVolumes(context.Background(), true)
	require.NoError(t, err)
	require.Len(t, vols, 2)
	assert.Equal(t, "v1", vols[0].Name)
	assert.Equal(t, "overlay", vols[0].Labels["io.balena.image.class"])
}

func TestRemoveVolume(t *testing.T) {
	called := false
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		called = true
		assert.Equal(t, "DELETE", method)
		assert.Equal(t, "/volumes/v1", path)
		return 204, nil
	})
	eng := testEngine(sock)
	require.NoError(t, eng.RemoveVolume(context.Background(), "v1"))
	assert.True(t, called)
}

func TestCreateVolume(t *testing.T) {
	sock := testServer(t, func(method, path string, body []byte) (int, []byte) {
		assert.Equal(t, "POST", method)
		assert.Equal(t, "/volumes/create", path)
		assert.JSONEq(t, `{"Name":"ext_kernel-modules_42befc76f4f8_boot",
			"Labels":{"io.balena.image.class":"overlay"}}`, string(body))
		return 201, []byte(`{"Name":"ext_kernel-modules_42befc76f4f8_boot",
			"Mountpoint":"/var/lib/docker/volumes/ext_kernel-modules_42befc76f4f8_boot/_data",
			"Labels":{"io.balena.image.class":"overlay"}}`)
	})

	eng := testEngine(sock)
	vol, err := eng.CreateVolume(context.Background(), "ext_kernel-modules_42befc76f4f8_boot",
		map[string]string{"io.balena.image.class": "overlay"})
	require.NoError(t, err)
	assert.Equal(t, "ext_kernel-modules_42befc76f4f8_boot", vol.Name)
	assert.Equal(t, "/var/lib/docker/volumes/ext_kernel-modules_42befc76f4f8_boot/_data", vol.Mountpoint)
}

// TestCreateVolume_ExistingVolume pins the create-or-get behaviour the
// engine gives us: a name that already exists comes back as the existing
// volume, with the labels it was created with rather than the ones just
// passed. Fabrication depends on that not being a conflict.
func TestCreateVolume_ExistingVolume(t *testing.T) {
	sock := testServer(t, func(_, _ string, _ []byte) (int, []byte) {
		return 201, []byte(`{"Name":"ext_svc_abc123456789_boot",
			"Mountpoint":"/var/lib/docker/volumes/ext_svc_abc123456789_boot/_data",
			"Labels":{"io.balena.image.class":"overlay","io.balena.image.kernel-abi-id":"original"}}`)
	})

	eng := testEngine(sock)
	vol, err := eng.CreateVolume(context.Background(), "ext_svc_abc123456789_boot",
		map[string]string{"io.balena.image.kernel-abi-id": "ignored"})
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/docker/volumes/ext_svc_abc123456789_boot/_data", vol.Mountpoint)
	assert.Equal(t, "original", vol.Labels["io.balena.image.kernel-abi-id"],
		"labels are only applied at first creation; the existing volume keeps its own")
}

// TestCreateVolume_NoLabels asserts an empty label set is omitted from the
// body rather than serialised as null.
func TestCreateVolume_NoLabels(t *testing.T) {
	sock := testServer(t, func(_, _ string, body []byte) (int, []byte) {
		assert.JSONEq(t, `{"Name":"ext_svc_abc123456789_boot"}`, string(body))
		return 201, []byte(`{"Name":"ext_svc_abc123456789_boot","Mountpoint":"/mnt"}`)
	})

	eng := testEngine(sock)
	_, err := eng.CreateVolume(context.Background(), "ext_svc_abc123456789_boot", nil)
	require.NoError(t, err)
}

func TestCreateVolume_EngineError(t *testing.T) {
	sock := testServer(t, func(_, _ string, _ []byte) (int, []byte) {
		return 500, []byte("volume driver failed")
	})

	eng := testEngine(sock)
	_, err := eng.CreateVolume(context.Background(), "ext_svc_abc123456789_boot", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "volume driver failed")
}

// TestNewEngine_DockerHost pins which DOCKER_HOST values may redirect the
// socket.
func TestNewEngine_DockerHost(t *testing.T) {
	tests := []struct {
		dockerHost string
		want       string
	}{
		{"", defaultSocket},
		{"unix:///var/run/docker.sock", "/var/run/docker.sock"},
		{"/var/run/docker.sock", "/var/run/docker.sock"},
		{"tcp://docker:2375", defaultSocket},
		{"npipe:////./pipe/docker_engine", defaultSocket},
		// A scheme with nothing after it names no socket, and dialing the
		// empty string can only fail.
		{"unix://", defaultSocket},
		// Relative paths resolve against whatever working directory the
		// process happens to have, which for the runtime is containerd's.
		{"unix://run/docker.sock", defaultSocket},
		{"run/docker.sock", defaultSocket},
	}
	for _, tc := range tests {
		t.Run(tc.dockerHost, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.dockerHost)
			assert.Equal(t, tc.want, NewEngine().socket)
		})
	}
}
