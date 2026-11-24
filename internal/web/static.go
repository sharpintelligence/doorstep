package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
)

const assetHashLength = 12

//go:embed static/*.css static/*.js
var staticFS embed.FS

var assetTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
}

type staticFile struct {
	body        []byte
	contentType string
	version     string
}

type staticFiles map[string]staticFile

func loadStatic() (staticFiles, error) {
	entries, err := fs.ReadDir(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("web: read static files: %w", err)
	}
	files := make(staticFiles, len(entries))
	for _, e := range entries {
		contentType, ok := assetTypes[path.Ext(e.Name())]
		if !ok || e.IsDir() {
			continue
		}
		body, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("web: read static file %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		files[e.Name()] = staticFile{body: body, contentType: contentType, version: hex.EncodeToString(sum[:])[:assetHashLength]}
	}
	return files, nil
}

func (s staticFiles) url(name string) (string, error) {
	f, ok := s[name]
	if !ok {
		return "", fmt.Errorf("web: unknown asset %q", name)
	}
	return "/static/" + name + "?v=" + f.version, nil
}

func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request) {
	f, ok := h.static[r.PathValue("file")]
	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	if !ok {
		header.Set("Content-Type", "text/plain; charset=utf-8")
		header.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found\n"))
		return
	}
	header.Set("Content-Type", f.contentType)
	header.Set("Content-Length", strconv.Itoa(len(f.body)))
	if r.URL.Query().Get("v") == f.version {
		header.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		header.Set("Cache-Control", "no-cache")
	}
	header.Set("ETag", `"`+f.version+`"`)
	if r.Header.Get("If-None-Match") == `"`+f.version+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(f.body)
}
