package router

import (
	"errors"
	"fmt"

	"github.com/bmatcuk/doublestar/v4"
)

type Rule struct {
	Server   string
	Language string
	Pattern  string
}

type Router struct {
	rules []Rule
}

func New(rules []Rule) (*Router, error) {
	for i, rule := range rules {
		if rule.Pattern == "" {
			continue
		}
		if err := ValidatePattern(rule.Pattern); err != nil {
			return nil, fmt.Errorf("router: rule %d (server %q): %w", i, rule.Server, err)
		}
	}
	return &Router{rules: rules}, nil
}

func ValidatePattern(pattern string) error {
	if !doublestar.ValidatePattern(pattern) {
		return fmt.Errorf("invalid pattern %q: %w", pattern, errInvalidPattern)
	}
	return nil
}

var errInvalidPattern = errors.New("invalid doublestar pattern")

func (r *Router) Route(languageID, path string) (server string, ok bool) {
	for _, rule := range r.rules {
		if rule.Language != "" && rule.Language != languageID {
			continue
		}
		if rule.Pattern != "" {
			matched, err := doublestar.Match(rule.Pattern, path)
			if err != nil || !matched {
				continue
			}
		}
		return rule.Server, true
	}
	return "", false
}
