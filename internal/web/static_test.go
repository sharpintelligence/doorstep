package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestVendoredFilesMatchVersions(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("static/VERSIONS")
	if err != nil {
		t.Fatalf("read VERSIONS: %v", err)
	}
	checked := 0
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) != 6 {
			t.Fatalf("VERSIONS line %q: want file, package, version, license, source and sha256", line)
		}
		name, want := fields[0], fields[5]
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatalf("%s is listed in VERSIONS but not embedded: %v", name, err)
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s sha256 = %s, VERSIONS records %s", name, got, want)
		}
		if !strings.Contains(fields[4], fields[2]) {
			t.Errorf("%s: source %s does not name version %s", name, fields[4], fields[2])
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("VERSIONS lists no files")
	}
	if _, err := os.Stat("static/htmx.LICENSE"); err != nil {
		t.Errorf("htmx licence missing: %v", err)
	}
}

func TestServeStatic(t *testing.T) {
	t.Parallel()

	h, err := New(newFakes().options(io.Discard))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := newMux(t, newFakes().options(io.Discard))
	for name, want := range map[string]string{"app.css": "text/css; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "htmx.min.js": "text/javascript; charset=utf-8"} {
		u, err := h.static.url(name)
		if err != nil {
			t.Fatalf("url(%s): %v", name, err)
		}
		body, _ := staticFS.ReadFile("static/" + name)
		sum := sha256.Sum256(body)
		if !strings.HasSuffix(u, "?v="+hex.EncodeToString(sum[:])[:12]) {
			t.Errorf("url(%s) = %s, want the content hash", name, u)
		}
		rec := serve(t, mux, call{path: u})
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != want || rec.Body.String() != string(body) {
			t.Errorf("GET %s = %d %s", u, rec.Code, rec.Header().Get("Content-Type"))
		}
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("GET %s Cache-Control = %q", u, got)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s without nosniff", u)
		}
		stale := serve(t, mux, call{path: "/static/" + name + "?v=old"})
		if stale.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("stale version cached: %q", stale.Header().Get("Cache-Control"))
		}
		etag := rec.Header().Get("ETag")
		if again := serve(t, mux, call{path: u, header: map[string]string{"If-None-Match": etag}}); again.Code != http.StatusNotModified {
			t.Errorf("conditional GET = %d, want 304", again.Code)
		}
	}
	for _, path := range []string{"/static/VERSIONS", "/static/htmx.LICENSE", "/static/nope.js", "/static/..%2fweb.go"} {
		if rec := serve(t, mux, call{path: path}); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if _, err := h.static.url("missing.css"); err == nil {
		t.Error("url of a missing asset succeeded")
	}
}

var (
	inlineScript = regexp.MustCompile(`<script(?:\s[^>]*)?>`)
	inlineStyle  = regexp.MustCompile(`(?i)<style|\sstyle=|\son[a-z]+=`)
)

func TestPagesHaveNoInlineScriptOrStyle(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/", "/apps/blog", "/deployments/9", "/locks", "/audit", "/tokens"} {
		html := body(t, get(t, path, admin))
		for _, tag := range inlineScript.FindAllString(html, -1) {
			if !strings.Contains(tag, ` src="/static/`) {
				t.Errorf("%s: inline script %s", path, tag)
			}
		}
		if m := inlineStyle.FindString(html); m != "" {
			t.Errorf("%s: inline style or handler %q", path, m)
		}
		if !strings.Contains(html, `"allowEval":false`) || !strings.Contains(html, `"includeIndicatorStyles":false`) {
			t.Errorf("%s: htmx config missing", path)
		}
	}
}
