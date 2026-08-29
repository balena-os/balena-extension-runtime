package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
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
	ID string `json:"Id"`
	// Image is the reference the container was created from.
	Image string `json:"Image"`
	// ImageID is the content digest it resolved to.
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Labels  map[string]string `json:"Labels"`
	Mounts  []MountPoint      `json:"Mounts"`
}

// MountPoint is the subset of a container's mount entry we need.
type MountPoint struct {
	Type        string `json:"Type"`        // "volume", "bind", "tmpfs", ...
	Source      string `json:"Source"`      // host path backing the mount
	Destination string `json:"Destination"` // path inside the container
}

// Image is the subset of Docker's image JSON we need.
type Image struct {
	ID       string            `json:"Id"`
	Labels   map[string]string `json:"Labels"`
	RepoTags []string          `json:"RepoTags"`
}

type ContainerInspect struct {
	ID    string         `json:"Id"`
	State ContainerState `json:"State"`
}

type ContainerState struct {
	Status   string `json:"Status"`
	Error    string `json:"Error"`
	ExitCode int    `json:"ExitCode"`
}

// Engine talks to the Docker Engine API over a unix socket.
type Engine struct {
	socket string
}

// NewEngine returns an Engine connected to the Docker socket.
//
// DOCKER_HOST overrides the socket, but only when it names one by absolute
// path: either a "unix://" URL or a bare path.
func NewEngine() *Engine {
	sock := defaultSocket
	if path, ok := unixSocketPath(os.Getenv("DOCKER_HOST")); ok {
		sock = path
	}
	return &Engine{socket: sock}
}

// unixSocketPath returns the absolute socket path a DOCKER_HOST value names,
// and whether it named one at all.
func unixSocketPath(dockerHost string) (string, bool) {
	path := dockerHost
	if trimmed, ok := strings.CutPrefix(dockerHost, "unix://"); ok {
		path = trimmed
	}
	if !filepath.IsAbs(path) {
		return "", false
	}
	return path, true
}

// CheckSocket verifies the engine socket exists and is a unix socket,
// returning an actionable error if not. Callers should invoke this once
// at startup so a missing socket produces a clear diagnostic instead of
// a cryptic dial error buried in the first API call.
func (e *Engine) CheckSocket() error {
	info, err := os.Stat(e.socket)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: socket not found at %s (override via DOCKER_HOST=unix:///path/to/socket)", ErrEngineUnavailable, e.socket)
		}
		return fmt.Errorf("%w: stat %s: %w", ErrEngineUnavailable, e.socket, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a unix socket", ErrEngineUnavailable, e.socket)
	}
	return nil
}

// do sends an HTTP/1.0 request over the unix socket and returns the decoded
// response body.
//
// We deliberately avoid net/http entirely to avoid dependency creep
func (e *Engine) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", e.socket)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %w", ErrEngineUnavailable, e.socket, err)
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

	if err := writeRequest(conn, method, path, body); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	resp, err := readResponse(bufio.NewReader(conn))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Cap response size to avoid OOM on a buggy or malicious engine.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponseBytes+1)))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return nil, fmt.Errorf("engine: %s %s: response body exceeds %d bytes", method, path, maxResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return nil, &engineError{Status: resp.StatusCode, Method: method, Path: path, Body: string(respBody)}
	}
	return respBody, nil
}

// engineError is a response the engine refused. It carries the status code so
// a caller can tell "no such object" from a fault without matching on the
// message text.
type engineError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *engineError) Error() string {
	return fmt.Sprintf("engine: %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
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

// InspectContainer returns the per-container inspect payload for ID.
func (e *Engine) InspectContainer(ctx context.Context, id string) (*ContainerInspect, error) {
	data, err := e.do(ctx, "GET", fmt.Sprintf("/containers/%s/json", url.PathEscape(id)), nil)
	if err != nil {
		return nil, err
	}
	var ci ContainerInspect
	if err := json.Unmarshal(data, &ci); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	return &ci, nil
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

type Volume struct {
	Name       string            `json:"Name"`
	Mountpoint string            `json:"Mountpoint"`
	Labels     map[string]string `json:"Labels"`
}

type volumeListResponse struct {
	Volumes []Volume `json:"Volumes"`
}

// ListVolumes returns volumes from the engine. If danglingOnly is true the
// query is filtered to dangling=true: volumes with no container references.
func (e *Engine) ListVolumes(ctx context.Context, danglingOnly bool) ([]Volume, error) {
	path := "/volumes"
	if danglingOnly {
		path += "?filters=" + url.QueryEscape(`{"dangling":["true"]}`)
	}
	body, err := e.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	var resp volumeListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode volume list: %w", err)
	}
	return resp.Volumes, nil
}

// volumeCreateRequest is the POST /volumes/create body. Labels is omitted
// when empty so the engine is never handed a null it would have to interpret.
type volumeCreateRequest struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels,omitempty"`
}

// CreateVolume creates a named volume carrying labels and returns the
// engine's view of it, including the host path the volume is backed by.
//
// A name that already exists comes back as the existing volume rather
// than a conflict.
// Labels are only ever applied at first creation; the engine ignores
// them for a volume that already exists.
func (e *Engine) CreateVolume(ctx context.Context, name string, labels map[string]string) (*Volume, error) {
	body, err := json.Marshal(volumeCreateRequest{Name: name, Labels: labels})
	if err != nil {
		return nil, fmt.Errorf("encode volume create: %w", err)
	}
	data, err := e.do(ctx, "POST", "/volumes/create", body)
	if err != nil {
		return nil, err
	}
	var v Volume
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("decode volume create: %w", err)
	}
	return &v, nil
}

// RemoveVolume deletes a named volume. Errors are propagated as-is so the
// caller's removalErrs accumulator can capture them.
func (e *Engine) RemoveVolume(ctx context.Context, name string) error {
	_, err := e.do(ctx, "DELETE", "/volumes/"+url.PathEscape(name), nil)
	return err
}
