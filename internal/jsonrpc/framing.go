// Package jsonrpc implements a minimal, dependency-free JSON-RPC 2.0
// transport for the Language Server Protocol base protocol: header +
// Content-Length framed body over an io.ReadWriteCloser.
package jsonrpc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const headerSeparator = "\r\n\r\n"

var headerColon = []byte(":")
var contentLengthHeader = []byte("Content-Length")

// readFrame reads a single LSP base-protocol frame from r: a sequence of
// "Name: Value\r\n" headers terminated by a blank line, followed by a
// body of exactly Content-Length bytes. Content-Type (and any other
// header) is ignored except Content-Length, whose name is matched
// case-insensitively.
//
// buf is a caller-owned scratch buffer for the body; it is grown (not
// replaced with a smaller slice) as needed and the resulting buffer,
// sized exactly to Content-Length, is returned as body so the caller can
// stash it and pass it back into the next call to reuse the backing
// array. Reusing the buffer across frames is safe because
// encoding/json copies every json.RawMessage field out of the bytes it
// is given on unmarshal (see (*RawMessage).UnmarshalJSON:
// *m = append((*m)[0:0], data...)), so once Run's json.Unmarshal(body, &m)
// returns, m retains no pointer into body and the backing array is free
// to be overwritten by the next frame.
func readFrame(r *bufio.Reader, buf []byte) ([]byte, error) {
	contentLength := -1

	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, errors.New("jsonrpc: header line exceeds buffer size")
			}
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("jsonrpc: reading header line: %w", err)
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			// End of headers.
			break
		}
		name, value, ok := bytes.Cut(line, headerColon)
		if !ok {
			return nil, fmt.Errorf("jsonrpc: malformed header %q", line)
		}
		name = bytes.TrimSpace(name)
		value = bytes.TrimSpace(value)
		if bytes.EqualFold(name, contentLengthHeader) {
			n, err := strconv.Atoi(string(value))
			if err != nil {
				return nil, fmt.Errorf("jsonrpc: invalid Content-Length %q: %w", value, err)
			}
			contentLength = n
		}
		// Other headers (e.g. Content-Type) are ignored.
	}

	if contentLength < 0 {
		return nil, errors.New("jsonrpc: missing Content-Length header")
	}

	if cap(buf) < contentLength {
		buf = make([]byte, contentLength)
	}
	buf = buf[:contentLength]
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("jsonrpc: reading body of length %d: %w", contentLength, err)
	}
	return buf, nil
}

// writeFrame writes body as a single LSP base-protocol frame to w. buf is
// a caller-owned scratch buffer (typically reused across calls under the
// same lock that serializes writes) used to assemble the header and body
// into one contiguous write; *buf is updated to the (possibly grown)
// backing array for reuse on the next call.
func writeFrame(w io.Writer, buf *[]byte, body []byte) error {
	b := append((*buf)[:0], "Content-Length: "...)
	b = strconv.AppendInt(b, int64(len(body)), 10)
	b = append(b, headerSeparator...)
	b = append(b, body...)
	*buf = b

	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("jsonrpc: writing frame: %w", err)
	}
	return nil
}
