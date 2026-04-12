package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const defaultSocket = "/var/run/balena-engine.sock"

// Container is the subset of Docker's container JSON we need.
type Container struct {
	ID     string            `json:"Id"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// Image is the subset of Docker's image JSON we need.
type Image struct {
	ID       string            `json:"Id"`
	Labels   map[string]string `json:"Labels"`
	RepoTags []string          `json:"RepoTags"`
}

// CreateResponse is the Docker create container response.
type CreateResponse struct {
	ID string `json:"Id"`
}

// parseStatusCode extracts the numeric status code from an HTTP status line
// (e.g., "HTTP/1.1 200 OK\r\n" → 200).
func parseStatusCode(line string) (int, error) {
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 {
		return 0, fmt.Errorf("malformed status line: %q", line)
	}
	return strconv.Atoi(parts[1])
}

// Engine talks to the Docker Engine API over a unix socket.
type Engine struct {
	socket string
}

// NewEngine returns an Engine connected to the Docker socket.
// It honours the DOCKER_HOST env var (unix:// scheme only).
func NewEngine() *Engine {
	sock := defaultSocket
	if dh := os.Getenv("DOCKER_HOST"); dh != "" {
		sock = strings.TrimPrefix(dh, "unix://")
	}
	return &Engine{socket: sock}
}

// do sends an HTTP/1.1 request over the unix socket and returns the decoded
// response body. http.ReadResponse is used so chunked transfer-encoding is
// handled transparently.
func (e *Engine) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", e.socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Build and send the request.
	var reqBuf strings.Builder
	fmt.Fprintf(&reqBuf, "%s %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n", method, path)
	if body != nil {
		fmt.Fprintf(&reqBuf, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	reqBuf.WriteString("\r\n")
	if _, err := io.WriteString(conn, reqBuf.String()); err != nil {
		return nil, err
	}
	if body != nil {
		if _, err := conn.Write(body); err != nil {
			return nil, err
		}
	}

	// Use http.ReadResponse so chunked transfer-encoding is decoded for us.
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("engine: %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// ListContainers returns containers matching the given label filter.
func (e *Engine) ListContainers(ctx context.Context, labelFilter string) ([]Container, error) {
	filters := fmt.Sprintf(`{"label":[%q]}`, labelFilter)
	path := fmt.Sprintf("/containers/json?all=true&filters=%s", url.QueryEscape(filters))
	data, err := e.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var containers []Container
	return containers, json.Unmarshal(data, &containers)
}

// CreateContainer creates a container with the given image, runtime, and labels.
func (e *Engine) CreateContainer(ctx context.Context, image, runtime string, labels map[string]string, cmd []string) (string, error) {
	body := map[string]any{
		"Image":  image,
		"Labels": labels,
		"Cmd":    cmd,
	}
	if runtime != "" {
		body["HostConfig"] = map[string]any{
			"Runtime": runtime,
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	data, err := e.do(ctx, "POST", "/containers/create", payload)
	if err != nil {
		return "", err
	}
	var cr CreateResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", err
	}
	return cr.ID, nil
}

// StartContainer starts a container by ID.
func (e *Engine) StartContainer(ctx context.Context, id string) error {
	_, err := e.do(ctx, "POST", fmt.Sprintf("/containers/%s/start", id), nil)
	return err
}

// RemoveContainer force-removes a container by ID.
func (e *Engine) RemoveContainer(ctx context.Context, id string) error {
	_, err := e.do(ctx, "DELETE", fmt.Sprintf("/containers/%s?force=true&v=true", id), nil)
	return err
}

// ListImages returns images matching the given label filter.
func (e *Engine) ListImages(ctx context.Context, labelFilter string) ([]Image, error) {
	filters := fmt.Sprintf(`{"label":[%q]}`, labelFilter)
	path := fmt.Sprintf("/images/json?filters=%s", url.QueryEscape(filters))
	data, err := e.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var images []Image
	return images, json.Unmarshal(data, &images)
}

// RemoveImage force-removes an image by ID.
func (e *Engine) RemoveImage(ctx context.Context, id string) error {
	_, err := e.do(ctx, "DELETE", fmt.Sprintf("/images/%s?force=true", id), nil)
	return err
}
