package gateway

import (
	"encoding/json"
	"fmt"
)

var droppedCapabilityKeys = []string{
	"workspaceSymbolProvider",
	"executeCommandProvider",
	"workspace",
}

const fixedTextDocumentSync = `{"openClose":true,"change":1,"save":{"includeText":false}}`

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
