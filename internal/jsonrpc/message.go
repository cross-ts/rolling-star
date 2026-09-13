package jsonrpc

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type ID struct {
	Num   int64
	Str   string
	IsStr bool
}

func NewIntID(n int64) ID { return ID{Num: n} }

func NewStringID(s string) ID { return ID{Str: s, IsStr: true} }

func (id ID) MarshalJSON() ([]byte, error) {
	if id.IsStr {
		return json.Marshal(id.Str)
	}
	return strconv.AppendInt(nil, id.Num, 10), nil
}

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

type Error struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc: code %d: %s", e.Code, e.Message)
}

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

const jsonRPCVersion = "2.0"

func (m *Message) IsRequest() bool {
	return m.Method != "" && m.ID != nil
}

func (m *Message) IsNotification() bool {
	return m.Method != "" && m.ID == nil
}

func (m *Message) IsResponse() bool {
	return m.Method == ""
}

var nullResult = json.RawMessage("null")
