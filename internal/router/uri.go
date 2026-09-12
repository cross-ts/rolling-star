package router

import (
	"net/url"
	"path/filepath"
	"strings"
)

// PathForRouting converts a document URI into the path that Rule patterns
// match against.
//
// The URI is parsed so percent-encoding is decoded (%20 -> space); its Path
// component is used. Non-"file:" schemes are returned unchanged, as-is. If
// the resulting path is inside rootPath, the path relative to rootPath is
// returned; otherwise the absolute path is returned with its leading "/"
// stripped. The result is always forward-slash-separated.
//
// Windows drive letters are out of scope: this function assumes POSIX-style
// absolute paths, as produced by file: URIs on non-Windows platforms.
func PathForRouting(rootPath, docURI string) string {
	u, err := url.Parse(docURI)
	if err != nil {
		return docURI
	}
	if u.Scheme != "file" {
		return docURI
	}

	path := u.Path

	rel, err := filepath.Rel(rootPath, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}

	return filepath.ToSlash(strings.TrimPrefix(path, "/"))
}
