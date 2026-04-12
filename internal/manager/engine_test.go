package manager

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

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

func TestCreateContainer(t *testing.T) {
	sock := testServer(t, func(method, path string, body []byte) (int, []byte) {
		assert.Equal(t, "POST", method)
		assert.Equal(t, "/containers/create", path)

		// Verify request body structure.
		var req map[string]any
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "myimage:latest", req["Image"])
		lbls := req["Labels"].(map[string]any)
		assert.Equal(t, "overlay", lbls["io.balena.image.class"])
		hc := req["HostConfig"].(map[string]any)
		assert.Equal(t, "extension", hc["Runtime"])

		return 201, []byte(`{"Id":"new-container-id"}`)
	})

	eng := testEngine(sock)
	id, err := eng.CreateContainer(context.Background(), "myimage:latest", "extension",
		map[string]string{"io.balena.image.class": "overlay"}, []string{"none"})
	require.NoError(t, err)
	assert.Equal(t, "new-container-id", id)
}

func TestCreateContainer_NoRuntime(t *testing.T) {
	sock := testServer(t, func(method, path string, body []byte) (int, []byte) {
		var req map[string]any
		require.NoError(t, json.Unmarshal(body, &req))
		// No HostConfig when runtime is empty.
		_, hasHC := req["HostConfig"]
		assert.False(t, hasHC)
		return 201, []byte(`{"Id":"cid"}`)
	})

	eng := testEngine(sock)
	id, err := eng.CreateContainer(context.Background(), "img", "", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "cid", id)
}

func TestStartContainer(t *testing.T) {
	sock := testServer(t, func(method, path string, _ []byte) (int, []byte) {
		assert.Equal(t, "POST", method)
		assert.Equal(t, "/containers/abc123/start", path)
		return 204, nil
	})

	eng := testEngine(sock)
	require.NoError(t, eng.StartContainer(context.Background(), "abc123"))
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
