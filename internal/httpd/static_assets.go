package httpd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/js"
)

const (
	staticOriginalCache  = "private, no-cache"
	staticImmutableCache = "private, max-age=31536000, immutable"
)

type staticAsset struct {
	body         []byte
	gzipBody     []byte
	contentType  string
	etag         string
	cacheControl string
}

type staticAssetSet struct {
	byPath map[string]staticAsset
	err    error
}

func prepareStaticAssets(webFS fs.FS) *staticAssetSet {
	assets := &staticAssetSet{byPath: map[string]staticAsset{}}
	minifier := minify.New()
	minifier.AddFunc("text/javascript", js.Minify)
	minifier.AddFunc("text/css", css.Minify)

	sources := map[string]staticRoute{}
	for route, value := range staticRoutes {
		if route != "/" {
			sources[route] = value
		}
	}
	err := fs.WalkDir(webFS, "web/vendor", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		route := "/" + strings.TrimPrefix(name, "web/")
		sources[route] = staticRoute{file: route[1:], contentType: staticContentType(route)}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		assets.err = err
		return assets
	}

	productionPaths := map[string]string{}
	for original, route := range sources {
		raw, err := fs.ReadFile(webFS, "web/"+route.file)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			assets.err = err
			return assets
		}
		assets.add(original, raw, route.contentType, staticOriginalCache)
		body := raw
		mediaType := staticMediaType(route.file)
		if mediaType != "" {
			body, err = minifier.Bytes(mediaType, bytes.Clone(raw))
			if err != nil {
				assets.err = fmt.Errorf("minify %s: %w", route.file, err)
				return assets
			}
		}
		production := hashedStaticPath(original, body)
		productionPaths[original] = production
		assets.add(production, body, route.contentType, staticImmutableCache)
	}

	index, err := fs.ReadFile(webFS, "web/index.html")
	if err == nil {
		for original, production := range productionPaths {
			pattern := regexp.MustCompile(`"` + regexp.QuoteMeta(original) + `(?:\?[^"]*)?"`)
			index = pattern.ReplaceAll(index, []byte(`"`+production+`"`))
		}
		deferredPaths := map[string]string{}
		for original, production := range productionPaths {
			if staticMediaType(original) != "" && !bytes.Contains(index, []byte(`"`+production+`"`)) {
				deferredPaths[original] = production
			}
		}
		manifest, marshalErr := json.Marshal(deferredPaths)
		if marshalErr != nil {
			assets.err = fmt.Errorf("marshal static asset manifest: %w", marshalErr)
			return assets
		}
		marker := []byte(`<script id="bridge-assets" type="application/json">{}</script>`)
		replacement := []byte(`<script id="bridge-assets" type="application/json">` + string(manifest) + `</script>`)
		index = bytes.Replace(index, marker, replacement, 1)
		assets.add("/", index, "text/html; charset=utf-8", "no-store")
	} else if !errors.Is(err, fs.ErrNotExist) {
		assets.err = err
	}
	return assets
}

func (s *staticAssetSet) add(path string, body []byte, contentType, cacheControl string) {
	digest := sha256.Sum256(body)
	asset := staticAsset{
		body:         body,
		contentType:  contentType,
		etag:         `"` + hex.EncodeToString(digest[:]) + `"`,
		cacheControl: cacheControl,
	}
	if staticCompressible(contentType) && len(body) >= 1024 {
		compressed := gzipBytes(body)
		if len(compressed) < len(body) {
			asset.gzipBody = compressed
		}
	}
	s.byPath[path] = asset
}

func hashedStaticPath(path string, body []byte) string {
	digest := sha256.Sum256(body)
	ext := pathpkg.Ext(path)
	return strings.TrimSuffix(path, ext) + "." + hex.EncodeToString(digest[:6]) + ext
}

func staticMediaType(name string) string {
	switch filepath.Ext(name) {
	case ".js":
		return "text/javascript"
	case ".css":
		return "text/css"
	default:
		return ""
	}
}

func staticContentType(name string) string {
	switch filepath.Ext(name) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".woff", ".woff2":
		return "font/" + strings.TrimPrefix(filepath.Ext(name), ".")
	case ".ttf":
		return "font/ttf"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".webmanifest":
		return "application/manifest+json"
	default:
		return "application/octet-stream"
	}
}

func staticCompressible(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		strings.HasPrefix(contentType, "application/json") ||
		contentType == "application/manifest+json" || contentType == "image/svg+xml"
}

func (h *handler) serveStatic() bool {
	path := h.request.URL.Path
	if h.server.assets.err != nil {
		_, known := staticRoutes[path]
		if known || strings.HasPrefix(path, "/vendor/") {
			h.fail(http.StatusInternalServerError, errorBody("静态资源装配失败"))
			return true
		}
	}
	asset, ok := h.server.assets.byPath[path]
	if !ok {
		return false
	}
	if h.server.assets.err != nil {
		h.fail(http.StatusInternalServerError, errorBody("静态资源装配失败"))
		return true
	}
	header := h.writer.Header()
	header.Set("Cache-Control", asset.cacheControl)
	header.Set("ETag", asset.etag)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", h.contentSecurityPolicy())
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("Content-Type", asset.contentType)
	header.Add("Vary", "Accept-Encoding")
	if etagMatches(h.request.Header.Get("If-None-Match"), asset.etag) {
		h.writer.WriteHeader(http.StatusNotModified)
		return true
	}
	body := asset.body
	if len(asset.gzipBody) > 0 && gzipAccepted(h.request) {
		body = asset.gzipBody
		header.Set("Content-Encoding", "gzip")
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	h.writer.WriteHeader(http.StatusOK)
	if h.request.Method != http.MethodHead {
		_, _ = h.writer.Write(body)
	}
	return true
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
