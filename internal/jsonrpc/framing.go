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
