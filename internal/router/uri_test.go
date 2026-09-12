package router

import "testing"

func TestPathForRouting(t *testing.T) {
	cases := []struct {
		name string
		root string
		uri  string
		want string
	}{
		{"in-root", "/repo", "file:///repo/docker-compose.yml", "docker-compose.yml"},
		{"in-root nested", "/repo", "file:///repo/.github/workflows/ci.yml", ".github/workflows/ci.yml"},
		{"out-of-root", "/repo", "file:///elsewhere/a.yml", "elsewhere/a.yml"},
		{"exact-root", "/repo", "file:///repo", "."},
		{"prefix-collision", "/repo", "file:///repository/a.yml", "repository/a.yml"},
		{"percent-encoded", "/repo", "file:///repo/my%20dir/a.yml", "my dir/a.yml"},
		{"non-file scheme", "/repo", "untitled:Untitled-1", "untitled:Untitled-1"},
		{"empty authority", "/repo", "file:///x", "x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PathForRouting(tc.root, tc.uri)
			if got != tc.want {
				t.Errorf("PathForRouting(%q, %q) = %q, want %q", tc.root, tc.uri, got, tc.want)
			}
		})
	}
}
