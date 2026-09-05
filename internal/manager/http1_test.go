package manager

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readBody(t *testing.T, raw string) (*response, string) {
	t.Helper()
	resp, err := readResponse(bufio.NewReader(strings.NewReader(raw)))
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(b)
}

func TestReadResponse_RefusesChunked(t *testing.T) {
	// HTTP/1.0 forbids it; if a server does it anyway, say so rather than
	// handing back chunk headers as body.
	_, err := readResponse(bufio.NewReader(strings.NewReader(
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Transfer-Encoding")
}

func TestReadResponse_ContentLengthAndStatus(t *testing.T) {
	resp, body := readBody(t, "HTTP/1.1 404 Not Found\r\nContent-Length: 3\r\n\r\nnopEXTRA")
	assert.Equal(t, 404, resp.StatusCode)
	assert.Equal(t, "nop", body)
}

func TestReadResponse_RejectsNegativeContentLength(t *testing.T) {
	// LimitReader with a negative limit yields an empty body, which would
	// turn a malformed response into a silently successful one.
	_, err := readResponse(bufio.NewReader(strings.NewReader(
		"HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Content-Length")
}

func TestReadResponse_RejectsNonHTTPStatusLine(t *testing.T) {
	// Dialing something that is not an HTTP server should fail here, not as
	// a JSON decode error downstream.
	_, err := readResponse(bufio.NewReader(strings.NewReader(
		"SSH-2.0-OpenSSH 9\r\n\r\n")))
	require.Error(t, err)
}

func TestReadResponse_EOFDelimitedBody(t *testing.T) {
	// No Content-Length: the body runs to connection close. This is how the
	// engine answers when it streams instead of buffering.
	resp, body := readBody(t, "HTTP/1.0 200 OK\r\n\r\n{\"ok\":true}")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, `{"ok":true}`, body)
}

// Cross-check the hand-rolled parser against responses produced by the stdlib
// server, which is the closest available stand-in for the engine's framing.
func TestReadResponse_MatchesStdlibServer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		h          http.HandlerFunc
		wantStatus int
		want       string
	}{
		{"chunked", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			for i := 0; i < 3; i++ {
				_, _ = io.WriteString(w, strings.Repeat("x", 700))
				w.(http.Flusher).Flush()
			}
		}, 200, strings.Repeat("x", 2100)},
		{"sized", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"ok":true}`)
		}, 200, `{"ok":true}`},
		{"no content", func(w http.ResponseWriter, r *http.Request) {
			// The DELETE endpoints answer 204 with neither Content-Length
			// nor a body, so this exercises the read-to-EOF branch.
			w.WriteHeader(http.StatusNoContent)
		}, 204, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, writeRequest(conn, "GET", "/", nil))
			resp, err := readResponse(bufio.NewReader(conn))
			require.NoError(t, err)
			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			assert.Equal(t, tc.want, string(b))
		})
	}
}
