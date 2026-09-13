package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/cross-ts/rolling-star/internal/router"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Servers []ServerDef `yaml:"servers"`
}

type ServerDef struct {
	Name string `yaml:"name"`

	Command string `yaml:"command"`

	Args []string `yaml:"args"`

	Env map[string]string `yaml:"env"`

	InitializationOptions any `yaml:"initializationOptions"`

	Selectors []Selector `yaml:"selectors"`
}

type Selector struct {
	Language string `yaml:"language"`

	Pattern string `yaml:"pattern"`
}

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
