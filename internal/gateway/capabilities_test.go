package gateway

import (
	"encoding/json"
	"testing"
)

func TestMergeCapabilities(t *testing.T) {
	caps := []json.RawMessage{
		json.RawMessage(`{
			"hoverProvider": true,
			"definitionProvider": false,
			"documentSymbolProvider": "custom-value-a",
			"workspaceSymbolProvider": true,
			"executeCommandProvider": {"commands":["a"]},
			"workspace": {"workspaceFolders":{"supported":true}},
			"textDocumentSync": 2
		}`),
		json.RawMessage(`{
			"hoverProvider": false,
			"definitionProvider": true,
			"documentSymbolProvider": "custom-value-b",
			"renameProvider": true,
			"workspace": {"fileOperations":{}}
		}`),
	}

	merged, err := mergeCapabilities(caps)
	if err != nil {
		t.Fatalf("mergeCapabilities: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	// Bool OR across servers.
	if v, ok := asJSONBool(got["hoverProvider"]); !ok || !v {
		t.Errorf("hoverProvider = %s, want true (OR of true,false)", got["hoverProvider"])
	}
	if v, ok := asJSONBool(got["definitionProvider"]); !ok || !v {
		t.Errorf("definitionProvider = %s, want true (OR of false,true)", got["definitionProvider"])
	}

	// First-writer-wins for non-bool values.
	if string(got["documentSymbolProvider"]) != `"custom-value-a"` {
		t.Errorf("documentSymbolProvider = %s, want first writer's value", got["documentSymbolProvider"])
	}

	// Key only present in the second server is still copied.
	if v, ok := asJSONBool(got["renameProvider"]); !ok || !v {
		t.Errorf("renameProvider = %s, want true", got["renameProvider"])
	}

	// Dropped workspace-scoped keys.
	for _, k := range []string{"workspaceSymbolProvider", "executeCommandProvider", "workspace"} {
		if _, present := got[k]; present {
			t.Errorf("key %q should have been dropped, got %s", k, got[k])
		}
	}

	// Hard override regardless of what servers advertised.
	if string(got["textDocumentSync"]) != fixedTextDocumentSync {
		t.Errorf("textDocumentSync = %s, want %s", got["textDocumentSync"], fixedTextDocumentSync)
	}
}

func TestMergeCapabilities_Empty(t *testing.T) {
	merged, err := mergeCapabilities(nil)
	if err != nil {
		t.Fatalf("mergeCapabilities(nil): %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got["textDocumentSync"]) != fixedTextDocumentSync {
		t.Errorf("textDocumentSync = %s, want %s", got["textDocumentSync"], fixedTextDocumentSync)
	}
}
