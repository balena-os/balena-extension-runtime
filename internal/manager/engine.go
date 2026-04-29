package manager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultSocket  = "/var/run/balena-engine.sock"
	defaultTimeout = 30 * time.Second
)

// maxResponseBytes is a var (not const) so tests can lower the cap to
// exercise the size-limit path without allocating the real 32 MiB.
var maxResponseBytes = 32 << 20 // 32 MiB

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

// CheckSocket verifies the engine socket exists and is a unix socket,
// returning an actionable error if not. Callers should invoke this once
// at startup so a missing socket produces a clear diagnostic instead of
// a cryptic dial error buried in the first API call.
func (e *Engine) CheckSocket() error {
	info, err := os.Stat(e.socket)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("balena-engine socket not found at %s (override via DOCKER_HOST=unix:///path/to/socket)", e.socket)
		}
		return fmt.Errorf("stat %s: %w", e.socket, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a unix socket", e.socket)
	}
	return nil
}

// do sends an HTTP/1.1 request over the unix socket and returns the decoded
// response body.
//
// We deliberately avoid http.Client / http.Transport: Transport's reachable
// call graph drags crypto/tls and HTTP/2 into the binary, neither of which
// we need for a unix-socket transport. Instead we dial directly and use
// net/http's low-level primitives — http.Request.Write for serialising the
// request and http.ReadResponse for parsing chunked/length-delimited
// replies. These stay TLS-free while still giving us stdlib-grade
// correctness for the tricky parts.
func (e *Engine) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", e.socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Cap the total request/response time. Honour an earlier ctx deadline
	// if the caller set one; otherwise apply a ceiling so a hung daemon
	// can't block indefinitely.
	deadline := time.Now().Add(defaultTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	// Propagate ctx cancellation to in-flight reads/writes by closing the
	// conn when ctx fires. Without this, cancellation only takes effect
	// when the deadline above expires.
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()

	// Build the request. Host "localhost" is arbitrary (ignored by the
	// engine, which only cares about the path). http.NewRequestWithContext
	// parses the URL and rejects CRLF in the path for us.
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Close = true
	if body != nil {
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// http.ReadResponse decodes chunked transfer-encoding, trailers, and
	// content-length framing for us.
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

	// Cap response size to avoid OOM on a buggy or malicious engine.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponseBytes+1)))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return nil, fmt.Errorf("engine: %s %s: response body exceeds %d bytes", method, path, maxResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("engine: %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// labelFilterQuery builds the url-encoded `filters` query value that the
// Docker Engine API expects for `label=<filter>` selection.
func labelFilterQuery(labelFilter string) string {
	return url.QueryEscape(fmt.Sprintf(`{"label":[%q]}`, labelFilter))
}

// ListContainers returns containers matching the given label filter.
func (e *Engine) ListContainers(ctx context.Context, labelFilter string) ([]Container, error) {
	path := fmt.Sprintf("/containers/json?all=true&filters=%s", labelFilterQuery(labelFilter))
	data, err := e.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var containers []Container
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("decode containers: %w", err)
	}
	return containers, nil
}

// RemoveContainer force-removes a container by ID.
func (e *Engine) RemoveContainer(ctx context.Context, id string) error {
	_, err := e.do(ctx, "DELETE", fmt.Sprintf("/containers/%s?force=true&v=true", url.PathEscape(id)), nil)
	return err
}

// ListImages returns images matching the given label filter.
func (e *Engine) ListImages(ctx context.Context, labelFilter string) ([]Image, error) {
	path := fmt.Sprintf("/images/json?filters=%s", labelFilterQuery(labelFilter))
	data, err := e.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var images []Image
	if err := json.Unmarshal(data, &images); err != nil {
		return nil, fmt.Errorf("decode images: %w", err)
	}
	return images, nil
}

// RemoveImage force-removes an image by ID.
func (e *Engine) RemoveImage(ctx context.Context, id string) error {
	_, err := e.do(ctx, "DELETE", fmt.Sprintf("/images/%s?force=true", url.PathEscape(id)), nil)
	return err
}
