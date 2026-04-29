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
