package manager

import (
	"bufio"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
)

// response is the part of an HTTP/1.1 reply this client acts on.
type response struct {
	StatusCode int
	Header     textproto.MIMEHeader
	Body       io.Reader
}

func writeRequest(w io.Writer, method, path string, body []byte) error {
	if strings.ContainsAny(path, "\r\n") {
		return fmt.Errorf("request path contains a line break")
	}
	// Connection: close is the HTTP/1.0 default, but the unsized-body path in
	// readResponse depends on the server closing; ask explicitly rather than
	// relying on the default.
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n", method, path)
	if body != nil {
		fmt.Fprintf(&b, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if body != nil {
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

func readResponse(r *bufio.Reader) (*response, error) {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("read status line: %w", err)
	}
	// The proto check catches dialing something that is not an HTTP server
	// at all, which would otherwise surface as a JSON decode error downstream.
	proto, rest, ok := strings.Cut(line, " ")
	if !ok || !strings.HasPrefix(proto, "HTTP/") {
		return nil, fmt.Errorf("malformed status line %q", line)
	}
	codeText, _, _ := strings.Cut(rest, " ")
	code, err := strconv.Atoi(codeText)
	if err != nil {
		return nil, fmt.Errorf("malformed status code in %q", line)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("read headers: %w", err)
	}
	resp := &response{StatusCode: code, Header: hdr}
	switch {
	case hdr.Get("Transfer-Encoding") != "":
		// The request is HTTP/1.0, which a conforming server may not answer with
		// chunked framing. Refusing is better than handing the caller chunk
		// headers embedded in what it will decode as JSON.
		return nil, fmt.Errorf("engine answered an HTTP/1.0 request with Transfer-Encoding %q",
			hdr.Get("Transfer-Encoding"))
	case hdr.Get("Content-Length") != "":
		n, err := strconv.ParseInt(hdr.Get("Content-Length"), 10, 64)
		if err != nil || n < 0 {
			// A negative length would make LimitReader yield an empty body,
			// turning a malformed response into a silently successful one.
			return nil, fmt.Errorf("malformed Content-Length %q", hdr.Get("Content-Length"))
		}
		resp.Body = io.LimitReader(r, n)
	default:
		resp.Body = r
	}
	return resp, nil
}
