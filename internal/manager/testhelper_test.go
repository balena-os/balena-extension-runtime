package manager

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// withModulesDir creates a temp rootfs with the given directory names under lib/modules/.
func withModulesDir(t *testing.T, names ...string) string {
	t.Helper()
	rootfs := t.TempDir()
	modulesDir := filepath.Join(rootfs, "lib", "modules")
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	for _, name := range names {
		if err := os.Mkdir(filepath.Join(modulesDir, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	return rootfs
}
