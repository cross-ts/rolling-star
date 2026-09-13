package jsonrpc

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ID is a JSON-RPC request/response id. It supports both the numeric and
// string forms permitted by the JSON-RPC 2.0 spec (and used by LSP).
type ID struct {
	Num   int64
	Str   string
	IsStr bool
}

// NewIntID returns a numeric ID.
func NewIntID(n int64) ID { return ID{Num: n} }

// NewStringID returns a string ID.
func NewStringID(s string) ID { return ID{Str: s, IsStr: true} }

// MarshalJSON implements json.Marshaler.
func (id ID) MarshalJSON() ([]byte, error) {
	if id.IsStr {
		return json.Marshal(id.Str)
	}
	return strconv.AppendInt(nil, id.Num, 10), nil
}

// UnmarshalJSON implements json.Unmarshaler, accepting either a JSON
// number or a JSON string.
func (id *ID) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*id = ID{Num: n}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*id = ID{Str: s, IsStr: true}
		return nil
	}
	return fmt.Errorf("jsonrpc: id is neither a number nor a string: %s", data)
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc: code %d: %s", e.Code, e.Message)
}

// Standard JSON-RPC / LSP error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Message represents a JSON-RPC request, notification, or response.
// A single struct deliberately covers all three shapes: this package is a
// relay, and splitting them into separate types would only add
// conversions at every boundary without buying any safety.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// jsonRPCVersion is the only JSON-RPC version this package speaks.
const jsonRPCVersion = "2.0"

// IsRequest reports whether m is a request (has both a method and an id).
func (m *Message) IsRequest() bool {
	return m.Method != "" && m.ID != nil
}

// IsNotification reports whether m is a notification (has a method but no id).
func (m *Message) IsNotification() bool {
	return m.Method != "" && m.ID == nil
}

// IsResponse reports whether m is a response (no method: either a result
// or an error, correlated by id).
func (m *Message) IsResponse() bool {
	return m.Method == ""
}

// nullResult is the JSON encoding of a null result, used so that a reply
// with a nil result still marshals an explicit "result": null field
// rather than omitting result entirely (which would make the response
// look malformed to strict peers).
var nullResult = json.RawMessage("null")
