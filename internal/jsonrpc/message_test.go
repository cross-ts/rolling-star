package jsonrpc

import (
	"encoding/json"
	"testing"
)

func TestIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   ID
	}{
		{"numeric", NewIntID(42)},
		{"numeric zero", NewIntID(0)},
		{"string", NewStringID("abc-123")},
		{"string empty", NewStringID("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.id)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got ID
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got != tt.id {
				t.Fatalf("round trip mismatch: got %+v want %+v", got, tt.id)
			}
		})
	}
}

func TestIDMarshalForm(t *testing.T) {
	data, err := json.Marshal(NewIntID(7))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != "7" {
		t.Fatalf("numeric id should marshal bare, got %s", data)
	}

	data, err = json.Marshal(NewStringID("x"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != `"x"` {
		t.Fatalf("string id should marshal quoted, got %s", data)
	}
}

func TestIDUnmarshalInvalid(t *testing.T) {
	var id ID
	if err := json.Unmarshal([]byte("true"), &id); err == nil {
		t.Fatal("expected error unmarshaling a bool into ID")
	}
}

func TestMessagePredicates(t *testing.T) {
	id := NewIntID(1)

	tests := []struct {
		name             string
		m                Message
		wantRequest      bool
		wantNotification bool
		wantResponse     bool
	}{
		{
			name:        "request",
			m:           Message{Method: "textDocument/hover", ID: &id},
			wantRequest: true,
		},
		{
			name:             "notification",
			m:                Message{Method: "textDocument/didOpen"},
			wantNotification: true,
		},
		{
			name:         "response with result",
			m:            Message{ID: &id, Result: json.RawMessage(`{}`)},
			wantResponse: true,
		},
		{
			name:         "response with error",
			m:            Message{ID: &id, Error: &Error{Code: CodeInternalError, Message: "boom"}},
			wantResponse: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.IsRequest(); got != tt.wantRequest {
				t.Errorf("IsRequest() = %v, want %v", got, tt.wantRequest)
			}
			if got := tt.m.IsNotification(); got != tt.wantNotification {
				t.Errorf("IsNotification() = %v, want %v", got, tt.wantNotification)
			}
			if got := tt.m.IsResponse(); got != tt.wantResponse {
				t.Errorf("IsResponse() = %v, want %v", got, tt.wantResponse)
			}
		})
	}
}

// TestReplyNullResult verifies that Reply with a nil result still
// produces a JSON-RPC response object with an explicit "result": null,
// rather than omitting result entirely.
func TestReplyNullResult(t *testing.T) {
	msg := &Message{JSONRPC: jsonRPCVersion, ID: func() *ID { id := NewIntID(1); return &id }()}
	result := json.RawMessage(nil)
	if result == nil {
		result = nullResult
	}
	msg.Result = result

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	raw, ok := decoded["result"]
	if !ok {
		t.Fatalf("expected \"result\" field to be present, got %s", data)
	}
	if string(raw) != "null" {
		t.Fatalf("expected result to be null, got %s", raw)
	}
	if _, hasError := decoded["error"]; hasError {
		t.Fatalf("response should not carry both result and error: %s", data)
	}
}
