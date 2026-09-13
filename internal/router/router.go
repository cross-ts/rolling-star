// Package router implements the selector matching used to pick which
// downstream language server owns a given document. It is deliberately
// dependency-free (apart from doublestar) and holds no mutable state, so it
// can be tested as a pure table without processes, pipes, or JSON-RPC.
package router

import (
	"errors"
	"fmt"

	"github.com/bmatcuk/doublestar/v4"
)

// Rule is one selector entry, already flattened from config.
type Rule struct {
	Server   string // the downstream server name this rule selects
	Language string // "" matches any languageId
	Pattern  string // "" matches any path; otherwise a doublestar glob
}

// Router matches (languageID, path) pairs against an ordered list of rules.
// It holds no root path and no mutable state: computing the path to match
// against is the caller's job, which keeps this type trivially testable.
type Router struct {
	rules []Rule
}

// New validates and compiles the given rules and returns a Router. It
// returns an error naming the offending rule index and server if any
// rule's Pattern is not a valid selector pattern.
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

// ValidatePattern reports whether pattern is a syntactically valid selector pattern.
func ValidatePattern(pattern string) error {
	if !doublestar.ValidatePattern(pattern) {
		return fmt.Errorf("invalid pattern %q: %w", pattern, errInvalidPattern)
	}
	return nil
}

var errInvalidPattern = errors.New("invalid doublestar pattern")

// Route performs a linear scan over the rules in order and returns the
// server name of the first rule whose Language matches languageID (or is
// empty) and whose Pattern matches path (or is empty). If no rule matches,
// it returns ("", false).
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
