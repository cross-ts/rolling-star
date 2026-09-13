// Package config loads and validates the rolling-star YAML configuration
// file that lists downstream language servers and the selectors used to
// route documents to them.
//
// Two semantics are deliberate v1 decisions and are documented here rather
// than in code comments scattered across the package:
//
//  1. Matching is first-match-wins in config order. Selectors are not
//     required to be mutually exclusive; order is significant. This is what
//     lets a generic YAML fallback be written as a plain "**/*.{yml,yaml}"
//     pattern placed after a more specific entry, since "not under
//     .github/workflows" cannot itself be expressed as a glob.
//  2. Patterns match the workspace-root-relative path, not the document URI
//     and not an absolute filesystem path. The actual matching logic lives
//     in internal/router; this package only validates pattern syntax.
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/cross-ts/rolling-star/internal/router"
	"gopkg.in/yaml.v3"
)

// Config is the top-level rolling-star configuration.
type Config struct {
	// Servers lists the downstream language servers, in the order used for
	// first-match-wins selector routing.
	Servers []ServerDef `yaml:"servers"`
}

// ServerDef describes one downstream language server: how to launch it and
// which documents should be routed to it.
type ServerDef struct {
	// Name identifies the server. Must be non-empty and unique within Config.
	Name string `yaml:"name"`

	// Command is the executable to launch for this server.
	Command string `yaml:"command"`

	// Args are passed to Command as-is.
	Args []string `yaml:"args"`

	// Env holds additional environment variables for the child process.
	// Each value has os.ExpandEnv applied to it when the config is loaded.
	Env map[string]string `yaml:"env"`

	// InitializationOptions is passed through verbatim as the server's LSP
	// initializationOptions. It is decoded by yaml.v3 as map[string]any (for
	// mapping nodes), which is directly json.Marshal-able with no
	// conversion step. Unlike Env, this field is NOT expanded in v1 — its
	// values are used exactly as written in the YAML file.
	InitializationOptions any `yaml:"initializationOptions"`

	// Selectors determine which documents route to this server. A server
	// must have at least one selector.
	Selectors []Selector `yaml:"selectors"`
}

// Selector matches a document by languageId and/or path glob. At least one
// of Language or Pattern must be non-empty.
type Selector struct {
	// Language is the LSP languageId to match. Empty means any languageId.
	Language string `yaml:"language"`

	// Pattern is a doublestar glob matched against the workspace-root-relative
	// path (see package doc). Empty means any path.
	Pattern string `yaml:"pattern"`
}

// Load reads the YAML configuration file at path, decodes it with strict
// (unknown-field-rejecting) parsing, expands environment variables in each
// server's Env values, and validates the result.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}

	for i := range cfg.Servers {
		for k, v := range cfg.Servers[i].Env {
			cfg.Servers[i].Env[k] = os.ExpandEnv(v)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	return &cfg, nil
}

// Validate checks that c describes a usable configuration: at least one
// server, unique non-empty names, non-empty commands, at least one selector
// per server, each selector specifying language and/or pattern, and each
// non-empty pattern being syntactically valid.
func (c *Config) Validate() error {
	if len(c.Servers) == 0 {
		return errors.New("no servers defined")
	}

	seen := make(map[string]bool, len(c.Servers))
	for i, s := range c.Servers {
		if s.Name == "" {
			return fmt.Errorf("server[%d]: name is empty", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("server[%d] (%s): duplicate name", i, s.Name)
		}
		seen[s.Name] = true

		if s.Command == "" {
			return fmt.Errorf("server[%d] (%s): command is empty", i, s.Name)
		}

		if len(s.Selectors) == 0 {
			return fmt.Errorf("server[%d] (%s): no selectors defined", i, s.Name)
		}

		for j, sel := range s.Selectors {
			if sel.Language == "" && sel.Pattern == "" {
				return fmt.Errorf("server[%d] (%s): selector[%d]: neither language nor pattern set", i, s.Name, j)
			}
			if sel.Pattern != "" {
				if err := router.ValidatePattern(sel.Pattern); err != nil {
					return fmt.Errorf("server[%d] (%s): selector[%d]: invalid pattern %q: %w", i, s.Name, j, sel.Pattern, err)
				}
			}
		}
	}

	return nil
}
