package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLoad_Valid(t *testing.T) {
	cfg, err := Load("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(cfg.Servers))
	}

	actions := cfg.Servers[0]
	if actions.Name != "actions" {
		t.Errorf("Servers[0].Name = %q, want %q", actions.Name, "actions")
	}
	if actions.Command != "actions-languageserver" {
		t.Errorf("Servers[0].Command = %q, want %q", actions.Command, "actions-languageserver")
	}
	if len(actions.Args) != 1 || actions.Args[0] != "--stdio" {
		t.Errorf("Servers[0].Args = %v, want [--stdio]", actions.Args)
	}
	if len(actions.Selectors) != 1 || actions.Selectors[0].Language != "yaml" ||
		actions.Selectors[0].Pattern != ".github/workflows/**/*.{yml,yaml}" {
		t.Errorf("Servers[0].Selectors = %+v", actions.Selectors)
	}

	yamlSrv := cfg.Servers[1]
	if yamlSrv.Name != "yaml" {
		t.Errorf("Servers[1].Name = %q, want %q", yamlSrv.Name, "yaml")
	}

	// initializationOptions must round-trip to the expected JSON shape,
	// with no conversion step required by callers (see package doc).
	got, err := json.Marshal(yamlSrv.InitializationOptions)
	if err != nil {
		t.Fatalf("json.Marshal(InitializationOptions): %v", err)
	}
	want := `{"yaml":{"validate":true}}`
	if string(got) != want {
		t.Errorf("InitializationOptions JSON = %s, want %s", got, want)
	}
}

func TestLoad_EnvExpansion(t *testing.T) {
	t.Setenv("ROLLING_STAR_TEST_VAR", "expanded")

	cfg, err := Load("testdata/env.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.Servers[0].Env["FOO"]
	want := "prefix-expanded-suffix"
	if got != want {
		t.Errorf("Env[FOO] = %q, want %q", got, want)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("testdata/does_not_exist.yaml")
	if err == nil {
		t.Fatal("Load: expected error, got nil")
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		contains string
	}{
		{"no servers", "testdata/no_servers.yaml", "no servers"},
		{"empty name", "testdata/empty_name.yaml", "name is empty"},
		{"duplicate name", "testdata/duplicate_name.yaml", "dup"},
		{"empty command", "testdata/empty_command.yaml", "nocommand"},
		{"no selectors", "testdata/no_selectors.yaml", "noselectors"},
		{"empty selector", "testdata/empty_selector.yaml", "emptyselector"},
		{"invalid pattern", "testdata/invalid_pattern.yaml", "badpattern"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.file)
			if err == nil {
				t.Fatalf("Load(%s): expected error, got nil", tt.file)
			}
			if !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("Load(%s) error = %q, want substring %q", tt.file, err.Error(), tt.contains)
			}
		})
	}
}

func TestLoad_UnknownField(t *testing.T) {
	_, err := Load("testdata/unknown_field.yaml")
	if err == nil {
		t.Fatal("Load: expected error for unknown field, got nil")
	}
}

func TestLoad_Malformed(t *testing.T) {
	_, err := Load("testdata/malformed.yaml")
	if err == nil {
		t.Fatal("Load: expected error for malformed yaml, got nil")
	}
}
