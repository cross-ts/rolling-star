package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/cross-ts/rolling-star/internal/router"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Servers []LanguageServer `yaml:"servers"`
}

type LanguageServer struct {
	Name string `yaml:"name"`
	Command string `yaml:"command"`
	Args []string `yaml:"args"`
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

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Servers) == 0 {
		return errors.New("no servers defined")
	}

	seen := make(map[string]bool, len(c.Servers))
	for i, s := range c.Servers {
		if err := s.validate(); err != nil {
			if s.Name == "" {
				return fmt.Errorf("server[%d]: %w", i, err)
			}
			return fmt.Errorf("server[%d] (%s): %w", i, s.Name, err)
		}
		if seen[s.Name] {
			return fmt.Errorf("server[%d] (%s): duplicate name", i, s.Name)
		}
		seen[s.Name] = true
	}

	return nil
}

func (s LanguageServer) validate() error {
	if s.Name == "" {
		return errors.New("name is empty")
	}

	if s.Command == "" {
		return errors.New("command is empty")
	}

	if len(s.Selectors) == 0 {
		return errors.New("no selectors defined")
	}

	for i, selector := range s.Selectors {
		if err := selector.validate(); err != nil {
			return fmt.Errorf("selector[%d]: %w", i, err)
		}
	}

	return nil
}

func (s Selector) validate() error {
	if s.Language == "" && s.Pattern == "" {
		return errors.New("neither language nor pattern set")
	}
	if s.Pattern != "" {
		if err := router.ValidatePattern(s.Pattern); err != nil {
			return fmt.Errorf("invalid pattern %q: %w", s.Pattern, err)
		}
	}
	return nil
}
