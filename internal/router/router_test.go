package router

import "testing"

var yamlRules = []Rule{
	{Server: "actions", Language: "yaml", Pattern: ".github/workflows/**/*.{yml,yaml}"},
	{Server: "yaml", Language: "yaml", Pattern: "**/*.{yml,yaml}"},
}

func TestRoute_Table(t *testing.T) {
	r, err := New(yamlRules)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name       string
		root       string
		uri        string
		languageID string
		wantServer string
		wantOK     bool
	}{
		{"actions ci.yml", "/repo", "file:///repo/.github/workflows/ci.yml", "yaml", "actions", true},
		{"actions ci.yaml", "/repo", "file:///repo/.github/workflows/ci.yaml", "yaml", "actions", true},
		{"actions nested", "/repo", "file:///repo/.github/workflows/nested/reusable.yml", "yaml", "actions", true},
		{"yaml dependabot", "/repo", "file:///repo/.github/dependabot.yml", "yaml", "yaml", true},
		{"yaml docker-compose", "/repo", "file:///repo/docker-compose.yml", "yaml", "yaml", true},
		{"yaml k8s", "/repo", "file:///repo/k8s/deploy.yaml", "yaml", "yaml", true},
		{"markdown none", "/repo", "file:///repo/README.md", "markdown", "", false},

		{"language mismatch falls through", "/repo", "file:///repo/.github/workflows/ci.yml", "json", "", false},
		{"outside root fallback", "/repo", "file:///elsewhere/a.yml", "yaml", "yaml", true},
		{"percent-decoding", "/repo", "file:///repo/my%20dir/a.yml", "yaml", "yaml", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := PathForRouting(tc.root, tc.uri)
			gotServer, gotOK := r.Route(tc.languageID, path)
			if gotServer != tc.wantServer || gotOK != tc.wantOK {
				t.Errorf("Route(%q, %q) [path=%q] = (%q, %v), want (%q, %v)",
					tc.languageID, tc.uri, path, gotServer, gotOK, tc.wantServer, tc.wantOK)
			}
		})
	}
}

func TestRoute_EmptyLanguageMatchesAny(t *testing.T) {
	r, err := New([]Rule{{Server: "any-lang", Language: "", Pattern: "**/*.yml"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, lang := range []string{"yaml", "json", ""} {
		server, ok := r.Route(lang, "a.yml")
		if !ok || server != "any-lang" {
			t.Errorf("Route(%q, a.yml) = (%q, %v), want (any-lang, true)", lang, server, ok)
		}
	}
}

func TestRoute_EmptyPatternMatchesAny(t *testing.T) {
	r, err := New([]Rule{{Server: "any-path", Language: "yaml", Pattern: ""}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, path := range []string{"a.yml", "nested/deep/b.yaml", ""} {
		server, ok := r.Route("yaml", path)
		if !ok || server != "any-path" {
			t.Errorf("Route(yaml, %q) = (%q, %v), want (any-path, true)", path, server, ok)
		}
	}
}

func TestRoute_EmptyRuleSetNeverMatches(t *testing.T) {
	r, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server, ok := r.Route("yaml", "a.yml")
	if ok || server != "" {
		t.Errorf("Route on empty ruleset = (%q, %v), want (\"\", false)", server, ok)
	}
}

func TestNew_RejectsInvalidGlob(t *testing.T) {
	_, err := New([]Rule{{Server: "bad", Language: "yaml", Pattern: "["}})
	if err == nil {
		t.Fatal("New: expected error for invalid glob pattern, got nil")
	}
}

func TestRoute_FirstMatchWinsIsOrderDependent(t *testing.T) {
	swapped := []Rule{yamlRules[1], yamlRules[0]}
	r, err := New(swapped)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	server, ok := r.Route("yaml", ".github/workflows/ci.yml")
	if !ok || server != "yaml" {
		t.Errorf("Route with swapped rules = (%q, %v), want (yaml, true)", server, ok)
	}
}
