// Package jsonrpc implements a minimal, dependency-free JSON-RPC 2.0
// transport for the Language Server Protocol base protocol: header +
// Content-Length framed body over an io.ReadWriteCloser.
package jsonrpc

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const headerSeparator = "\r\n\r\n"

// readFrame reads a single LSP base-protocol frame from r: a sequence of
// "Name: Value\r\n" headers terminated by a blank line, followed by a
// body of exactly Content-Length bytes. Content-Type (and any other
// header) is ignored except Content-Length, whose name is matched
// case-insensitively.
func readFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := -1

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("jsonrpc: reading header line: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// End of headers.
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("jsonrpc: malformed header %q", line)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if strings.EqualFold(name, "Content-Length") {
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("jsonrpc: invalid Content-Length %q: %w", value, err)
			}
			contentLength = n
		}
		// Other headers (e.g. Content-Type) are ignored.
	}

	if contentLength < 0 {
		return nil, fmt.Errorf("jsonrpc: missing Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("jsonrpc: reading body of length %d: %w", contentLength, err)
	}
	return body, nil
}

// writeFrame writes body as a single LSP base-protocol frame to w.
func writeFrame(w io.Writer, body []byte) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Content-Length: %d%s", len(body), headerSeparator)
	buf.Write(body)
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("jsonrpc: writing frame: %w", err)
	}
	return nil
}
