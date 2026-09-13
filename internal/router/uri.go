package router

import (
	"net/url"
	"path/filepath"
	"strings"
)

func FilePath(uri string) (path string, ok bool) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", false
	}
	if u.Scheme != "file" {
		return "", false
	}
	return u.Path, true
}

func PathForRouting(rootPath, docURI string) string {
	path, ok := FilePath(docURI)
	if !ok {
		return docURI
	}

	rel, err := filepath.Rel(rootPath, path)
	if err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}

	return filepath.ToSlash(strings.TrimPrefix(path, "/"))
}
