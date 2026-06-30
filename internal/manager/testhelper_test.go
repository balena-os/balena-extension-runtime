package manager

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testServer starts a mock HTTP server on a Unix socket.
// handler receives the method, path, and request body, and returns a status code and response body.
// Returns the socket path. The server shuts down when t completes.
func testServer(t *testing.T, handler func(method, path string, body []byte) (int, []byte)) string {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go handleConn(conn, handler)
		}
	}()

	return sock
}

// testEngine returns an Engine pointing at the given socket path.
func testEngine(sock string) *Engine {
	return &Engine{socket: sock}
}

// testEngineEnv sets DOCKER_HOST to a unix socket and returns a cleanup function.
// Use this when testing code that calls NewEngine() internally.
func testEngineEnv(t *testing.T, sock string) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "unix://"+sock)
}

func handleConn(conn net.Conn, handler func(string, string, []byte) (int, []byte)) {
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Parse request line.
	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(requestLine), " ", 3)
	if len(parts) < 2 {
		return
	}
	method, path := parts[0], parts[1]

	// Parse headers to get Content-Length.
	contentLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			fmt.Sscanf(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]), "%d", &contentLength)
		}
	}

	// Read body.
	var body []byte
	if contentLength > 0 {
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
	}

	statusCode, respBody := handler(method, path, body)

	// Write response.
	statusText := "OK"
	if statusCode == 201 {
		statusText = "Created"
	} else if statusCode == 204 {
		statusText = "No Content"
	} else if statusCode == 404 {
		statusText = "Not Found"
	} else if statusCode == 500 {
		statusText = "Internal Server Error"
	}

	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s",
		statusCode, statusText, len(respBody), string(respBody))
	conn.Write([]byte(resp))
}

// engineStub is a mock balena-engine API for cleanup tests.
// All fields are mu-protected so tests can mutate
// state from a concurrent goroutine while Cleanup is running.
type engineStub struct {
	mu                sync.Mutex
	Containers        []Container
	Images            []Image
	Inspects          map[string]string
	InspectStatus     map[string]int
	ImagesListStatus  int
	RemovedContainers []string
	RemovedImages     []string
	Volumes           []Volume
	RemovedVolumes    []string
}

func newEngineStub() *engineStub {
	return &engineStub{
		Inspects:      map[string]string{},
		InspectStatus: map[string]int{},
	}
}

// handler returns a testServer handler bound to the stub's state. The
// returned closure takes the stub's lock for every request, so concurrent
// callers (e.g. a tweaker goroutine + Cleanup) are serialised on the
// stub's view of the world.
func (s *engineStub) handler() func(method, path string, body []byte) (int, []byte) {
	return func(method, path string, _ []byte) (int, []byte) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case method == "GET" && strings.HasPrefix(path, "/containers/json"):
			resp, _ := json.Marshal(s.Containers)
			return 200, resp
		case method == "GET" && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			if code, ok := s.InspectStatus[id]; ok {
				return code, []byte(`{"message":"injected"}`)
			}
			if body, ok := s.Inspects[id]; ok {
				return 200, []byte(body)
			}
			return 404, nil
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			id = strings.SplitN(id, "?", 2)[0]
			s.RemovedContainers = append(s.RemovedContainers, id)
			return 204, nil
		case method == "GET" && strings.HasPrefix(path, "/images/json"):
			if s.ImagesListStatus != 0 {
				return s.ImagesListStatus, []byte(`{"message":"injected"}`)
			}
			resp, _ := json.Marshal(s.Images)
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/images/"):
			id := strings.TrimPrefix(path, "/images/")
			id = strings.SplitN(id, "?", 2)[0]
			s.RemovedImages = append(s.RemovedImages, id)
			return 200, []byte("[]")
		case method == "GET" && strings.HasPrefix(path, "/volumes"):
			resp, _ := json.Marshal(struct {
				Volumes []Volume `json:"Volumes"`
			}{Volumes: s.Volumes})
			return 200, resp
		case method == "DELETE" && strings.HasPrefix(path, "/volumes/"):
			name := strings.TrimPrefix(path, "/volumes/")
			name = strings.SplitN(name, "?", 2)[0]
			s.RemovedVolumes = append(s.RemovedVolumes, name)
			return 204, nil
		default:
			return 404, nil
		}
	}
}

// removedContainersSnapshot returns a copy of RemovedContainers taken
// under the stub's lock. Use this when reading from outside a request
// handler, e.g. after a concurrent test goroutine finishes.
func (s *engineStub) removedContainersSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.RemovedContainers))
	copy(out, s.RemovedContainers)
	return out
}

// inspectJSON builds a minimal ContainerInspect body. Most cleanup tests
// only need the State subfields.
func inspectJSON(id, status, errMsg string, exitCode int) string {
	return fmt.Sprintf(`{"Id":%q,"State":{"Status":%q,"Error":%q,"ExitCode":%d}}`,
		id, status, errMsg, exitCode)
}

