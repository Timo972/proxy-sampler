package api

import (
	"bytes"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	indexCacheControl     = "no-cache"
	immutableCacheControl = "public, max-age=31536000, immutable"
)

var hashedAssetName = regexp.MustCompile(`-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

type spaHandler struct {
	dist      fs.FS
	indexHTML []byte
}

func newSPAHandler(assets fs.FS) (http.Handler, error) {
	if assets == nil {
		return nil, fmt.Errorf("SPA assets are required")
	}
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil, fmt.Errorf("open SPA dist: %w", err)
	}
	indexHTML, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read SPA index: %w", err)
	}
	return &spaHandler{dist: dist, indexHTML: indexHTML}, nil
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	requested, ok := safeSPAPath(r.URL)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if requested == "" {
		h.serve(w, r, "index.html", h.indexHTML, indexCacheControl)
		return
	}

	info, err := fs.Stat(h.dist, requested)
	if err == nil && !info.IsDir() {
		content, readErr := fs.ReadFile(h.dist, requested)
		if readErr != nil {
			http.NotFound(w, r)
			return
		}
		cacheControl := indexCacheControl
		if strings.HasPrefix(requested, "assets/") && hashedAssetName.MatchString(path.Base(requested)) {
			cacheControl = immutableCacheControl
		}
		h.serve(w, r, requested, content, cacheControl)
		return
	}

	// Asset and extension-bearing requests are file requests, not client-side
	// routes. Returning the index for them would turn a missing script into HTML.
	if requested == "assets" || strings.HasPrefix(requested, "assets/") || path.Ext(requested) != "" {
		http.NotFound(w, r)
		return
	}
	h.serve(w, r, "index.html", h.indexHTML, indexCacheControl)
}

func safeSPAPath(value *url.URL) (string, bool) {
	escaped := value.EscapedPath()
	decoded, err := url.PathUnescape(escaped)
	if err != nil || !strings.HasPrefix(decoded, "/") || strings.ContainsAny(decoded, "\\\x00") {
		return "", false
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == ".." {
			return "", false
		}
	}
	requested := strings.TrimPrefix(decoded, "/")
	if requested == "" {
		return "", true
	}
	if !fs.ValidPath(requested) {
		return "", false
	}
	return requested, true
}

func (h *spaHandler) serve(w http.ResponseWriter, r *http.Request, name string, content []byte, cacheControl string) {
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
}

func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	}
	if value := mime.TypeByExtension(path.Ext(name)); value != "" {
		return value
	}
	return "application/octet-stream"
}
