package jsonrpc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	var wbuf []byte
	body := []byte(`{"jsonrpc":"2.0","method":"initialize","id":1}`)

	if err := writeFrame(&buf, &wbuf, body); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	got, err := readFrame(bufio.NewReader(&buf), nil)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip mismatch: got %q want %q", got, body)
	}
}

func TestFramingMultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	var wbuf []byte
	bodies := [][]byte{
		[]byte(`{"a":1}`),
		[]byte(`{"b":2}`),
		[]byte(`{}`),
	}
	for _, b := range bodies {
		if err := writeFrame(&buf, &wbuf, b); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}

	r := bufio.NewReader(&buf)
	var rbuf []byte
	for i, want := range bodies {
		got, err := readFrame(r, rbuf)
		if err != nil {
			t.Fatalf("readFrame[%d]: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d mismatch: got %q want %q", i, got, want)
		}
		rbuf = got
	}
	if _, err := readFrame(r, rbuf); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after last frame, got %v", err)
	}
}

func TestFramingExtraHeadersIgnored(t *testing.T) {
	body := `{"ok":true}`
	raw := "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"X-Custom: whatever\r\n" +
		"\r\n" + body

	got, err := readFrame(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != body {
		t.Fatalf("got %q want %q", got, body)
	}
}

func TestFramingHeaderNameCaseInsensitive(t *testing.T) {
	body := `{"ok":true}`
	raw := "content-length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body

	got, err := readFrame(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != body {
		t.Fatalf("got %q want %q", got, body)
	}
}

func TestFramingMissingContentLength(t *testing.T) {
	raw := "Content-Type: application/json\r\n\r\n{}"
	_, err := readFrame(bufio.NewReader(strings.NewReader(raw)), nil)
	if err == nil {
		t.Fatal("expected error for missing Content-Length")
	}
}

func TestFramingInvalidContentLength(t *testing.T) {
	raw := "Content-Length: not-a-number\r\n\r\n{}"
	_, err := readFrame(bufio.NewReader(strings.NewReader(raw)), nil)
	if err == nil {
		t.Fatal("expected error for invalid Content-Length")
	}
}

func TestFramingMalformedHeader(t *testing.T) {
	raw := "NotAHeaderLine\r\n\r\n{}"
	_, err := readFrame(bufio.NewReader(strings.NewReader(raw)), nil)
	if err == nil {
		t.Fatal("expected error for malformed header line")
	}
}

type slowReader struct {
	data []byte
	pos  int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}

	p[0] = s.data[s.pos]
	s.pos++
	time.Sleep(time.Microsecond)
	return 1, nil
}

func TestFramingBodySplitAcrossReads(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"a":1}}`
	raw := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body

	got, err := readFrame(bufio.NewReader(&slowReader{data: []byte(raw)}), nil)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != body {
		t.Fatalf("got %q want %q", got, body)
	}
}

func TestFramingTruncatedBody(t *testing.T) {
	raw := "Content-Length: 100\r\n\r\n{\"short\":true}"
	_, err := readFrame(bufio.NewReader(strings.NewReader(raw)), nil)
	if err == nil {
		t.Fatal("expected error for truncated body")
	}
}
