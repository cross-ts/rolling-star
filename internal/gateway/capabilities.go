package gateway

import (
	"encoding/json"
	"fmt"
)

// droppedCapabilityKeys are top-level capability keys removed after
// merging. All three are workspace-scoped, and rolling-star cannot route
// a workspace-level request to one specific downstream server in v1 (see
// the §4.4 policy: URI-less requests get MethodNotFound). Advertising
// these capabilities to the client would promise routing that does not
// exist, so they are dropped rather than merged.
var droppedCapabilityKeys = []string{
	"workspaceSymbolProvider",
	"executeCommandProvider",
	"workspace",
}

// fixedTextDocumentSync is hard-overridden onto the merged capabilities
// after merging. change:1 is TextDocumentSyncKind.Full: sending a
// full-document change notification is always legal for a server that
// asked for Incremental (2) too -- Full is a superset, not a competing
// choice -- so forcing it lets rolling-star relay didChange verbatim
// without tracking or recomputing per-server incremental diffs.
const fixedTextDocumentSync = `{"openClose":true,"change":1,"save":{"includeText":false}}`

// mergeCapabilities combines the "capabilities" objects returned by each
// downstream server's initialize response into the single object
// rolling-star reports to the upstream client, in caps's order (which
// callers should pass in config order, so config order settles ties).
//
// Per top-level key:
//   - a key not yet present in the merge is copied as-is
//   - if both the existing and incoming values are JSON booleans, they
//     are OR'd (so any server advertising a capability keeps it visible
//     even if an earlier server didn't have it)
//   - otherwise, the first server to write the key wins; later servers'
//     values for that key are ignored
//
// After merging, droppedCapabilityKeys are removed and textDocumentSync
// is overridden to fixedTextDocumentSync (see their doc comments).
func mergeCapabilities(caps []json.RawMessage) (json.RawMessage, error) {
	merged := make(map[string]json.RawMessage)

	for _, c := range caps {
		if len(c) == 0 {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(c, &obj); err != nil {
			return nil, fmt.Errorf("gateway: merge capabilities: %w", err)
		}
		for k, v := range obj {
			existing, ok := merged[k]
			if !ok {
				merged[k] = v
				continue
			}
			eb, eok := asJSONBool(existing)
			vb, vok := asJSONBool(v)
			if eok && vok {
				merged[k] = jsonBool(eb || vb)
			}
			// Otherwise: first writer wins; leave merged[k] as-is.
		}
	}

	for _, k := range droppedCapabilityKeys {
		delete(merged, k)
	}
	merged["textDocumentSync"] = json.RawMessage(fixedTextDocumentSync)

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("gateway: merge capabilities: %w", err)
	}
	return out, nil
}

// asJSONBool reports whether raw is a JSON boolean literal and, if so,
// its value.
func asJSONBool(raw json.RawMessage) (value bool, ok bool) {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	return b, true
}

func jsonBool(b bool) json.RawMessage {
	if b {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}
