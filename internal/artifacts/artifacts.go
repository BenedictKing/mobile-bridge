// Package artifacts resolves agent-produced files a chat references —
// screenshots, generated images, HTML visualizations — into servable,
// session-scoped descriptors. It is the Go port of bridge/files.py, extended
// to resolve relative markdown references against the session cwd.
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// MaxFile bounds regular artifacts; APKs need a larger limit for mobile builds.
const MaxFile = 50 * 1024 * 1024
const MaxAPKFile = 512 * 1024 * 1024

// markdownPath matches image and plain markdown links, angle-bracket targets
// included: `!?\[[^\]\n]*\]\((?:<target>|target)\)`.
var markdownPath = regexp.MustCompile(`!?\[[^\]\n]*\]\((?:<([^>\n]+)>|([^)\n]+))\)`)

var lineSuffix = regexp.MustCompile(`:[0-9]+$`)

var imageSuffixes = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// Entry is one servable artifact. Path stays gateway-side; the remaining
// fields are safe to expose through the session-scoped files payload.
type Entry struct {
	ID        string
	Path      string
	Reference string
	Name      string
	Image     bool
	Size      int64
	ModTime   int64
}

func IsAPK(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".apk")
}

func FileLimit(path string) int64 {
	if IsAPK(path) {
		return MaxAPKFile
	}
	return MaxFile
}

// Identity normalizes a local path for hashing and duplicate linking.
// Symlinks are resolved as far as they exist; missing tails keep the
// cleaned absolute form (Python's resolve(strict=False)).
func Identity(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}

// ID returns the 64-hex artifact id of a local path.
func ID(path string) string {
	sum := sha256.Sum256([]byte(Identity(path)))
	return hex.EncodeToString(sum[:])
}

// ReferencePath resolves one markdown/file reference to a local path:
// file:// URLs and plain absolute paths pass through, relative values join
// the session cwd, remote URLs return "". A trailing :line editor suffix is
// stripped for the lookup only.
func ReferencePath(value, cwd string) string {
	value = strings.TrimSpace(value)
	value = lineSuffix.ReplaceAllString(value, "")
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "file://") {
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Host != "" && parsed.Host != "localhost") || parsed.Path == "" {
			return ""
		}
		if decoded, err := url.PathUnescape(parsed.Path); err == nil {
			return filepath.Clean(decoded)
		}
		return ""
	}
	// Remote schemes carry "//" after the colon; plain Windows drive paths
	// and POSIX relatives do not.
	if remoteScheme.MatchString(value) {
		return ""
	}
	if decoded, err := url.PathUnescape(value); err == nil && decoded != "" {
		value = decoded
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	if cwd != "" {
		return filepath.Join(cwd, value)
	}
	return ""
}

var remoteScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// Roots lists the directories artifacts may live in for one session: the
// agent visualization directories that exist on this machine plus the cwd.
func Roots(cwd string) []string {
	roots := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		for _, candidate := range []string{
			filepath.Join(home, ".zcode", "v2", "visualizations"),
			filepath.Join(home, ".codex", "visualizations"),
		} {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				roots = append(roots, Identity(candidate))
			}
		}
	}
	if cwd != "" {
		if absolute, err := filepath.Abs(cwd); err == nil {
			roots = append(roots, Identity(absolute))
		}
	}
	return roots
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Collect gathers the artifacts a chat view references. Markdown links in
// assistant messages stay bound to the session roots (cwd and visualization
// directories); localImage/image attachments are runtime evidence and may
// point anywhere on disk. Keys are artifact IDs.
func Collect(view *jsoncompat.Object, cwd string) map[string]Entry {
	result := map[string]Entry{}
	roots := Roots(cwd)
	add := func(raw string, trusted bool) {
		path := ReferencePath(raw, cwd)
		if path == "" {
			return
		}
		resolved := Identity(path)
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Size() > FileLimit(resolved) {
			return
		}
		if !trusted {
			contained := false
			for _, root := range roots {
				if within(root, resolved) {
					contained = true
					break
				}
			}
			if !contained {
				return
			}
		}
		id := ID(resolved)
		if _, exists := result[id]; exists {
			return
		}
		result[id] = Entry{ID: id, Path: resolved, Reference: raw,
			Name: filepath.Base(resolved), Image: imageSuffixes[strings.ToLower(filepath.Ext(resolved))],
			Size: info.Size(), ModTime: info.ModTime().UnixMilli()}
	}
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return result
	}
	for _, turnValue := range turns.Items {
		turn, _ := turnValue.(*jsoncompat.Object)
		if turn == nil {
			continue
		}
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		if messages == nil {
			continue
		}
		for _, messageValue := range messages.Items {
			message, _ := messageValue.(*jsoncompat.Object)
			if message == nil {
				continue
			}
			role, _ := message.MustGet("role").(string)
			if text, _ := message.MustGet("text").(string); role == "assistant" && text != "" {
				for _, match := range markdownPath.FindAllStringSubmatch(text, -1) {
					reference := match[1]
					if reference == "" {
						reference = match[2]
					}
					add(reference, false)
				}
			}
			addAttachmentPaths(message.MustGet("attachments"), add)
		}
	}
	return result
}

// AttachmentPaths lists the local image paths a view's row attachments
// reference — the desktop-image fallback walk over the chat's own state.
func AttachmentPaths(view *jsoncompat.Object) []string {
	paths := []string{}
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return paths
	}
	cwd, _ := view.MustGet("cwd").(string)
	for _, turnValue := range turns.Items {
		turn, _ := turnValue.(*jsoncompat.Object)
		if turn == nil {
			continue
		}
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		if messages == nil {
			continue
		}
		for _, messageValue := range messages.Items {
			message, _ := messageValue.(*jsoncompat.Object)
			if message == nil {
				continue
			}
			addAttachmentPaths(message.MustGet("attachments"), func(raw string, _ bool) {
				if path := ReferencePath(raw, cwd); path != "" {
					paths = append(paths, path)
				}
			})
		}
	}
	return paths
}

func addAttachmentPaths(value any, add func(raw string, trusted bool)) {
	list, _ := value.(*jsoncompat.Array)
	if list == nil {
		return
	}
	for _, item := range list.Items {
		attachment, _ := item.(*jsoncompat.Object)
		if attachment == nil {
			continue
		}
		attachmentType, _ := attachment.MustGet("type").(string)
		path, _ := attachment.MustGet("path").(string)
		if path == "" {
			continue
		}
		switch attachmentType {
		case "localImage", "image":
			add(path, true)
		case "file":
			add(path, false)
		}
	}
}

// FilesPayload renders the files array timeline payloads carry: entries
// ordered by id so serialization is stable across reads.
func FilesPayload(entries map[string]Entry) *jsoncompat.Array {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	array := &jsoncompat.Array{}
	for _, id := range ids {
		entry := entries[id]
		object := jsoncompat.NewObject()
		object.Set("id", entry.ID)
		object.Set("name", entry.Name)
		object.Set("reference", entry.Reference)
		object.Set("image", entry.Image)
		object.Set("size", entry.Size)
		object.Set("modifiedAt", entry.ModTime)
		array.Items = append(array.Items, object)
	}
	return array
}

// SniffImage returns the image mime for known magic bytes, or "".
func SniffImage(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && string(data[:3]) == "\xff\xd8\xff":
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// Serve describes how one artifact file is served: its content type plus the
// sandbox policy for document types (agent-authored HTML needs its scripts
// but must never inherit the gateway's authenticated origin; SVG runs no
// scripts at all).
type Serve struct {
	Mime    string
	Sandbox string
}

// ServeType picks the response shape for an artifact. Raster images are
// sniffed; HTML previews get sandbox allow-scripts; anything unrecognized
// downloads as octet-stream. Sandboxed documents also declare frame-ancestors
// 'self' so only gateway pages (the chat's iframe preview) may frame them —
// these responses skip X-Frame-Options, which would otherwise block that
// same-origin preview entirely.
func ServeType(path string, data []byte) Serve {
	if mime := SniffImage(data); mime != "" {
		return Serve{Mime: mime}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return Serve{Mime: "text/html; charset=utf-8", Sandbox: "sandbox allow-scripts; frame-ancestors 'self'"}
	case ".svg":
		return Serve{Mime: "image/svg+xml", Sandbox: "sandbox; frame-ancestors 'self'"}
	case ".txt", ".md", ".log":
		return Serve{Mime: "text/plain; charset=utf-8"}
	case ".json":
		return Serve{Mime: "application/json"}
	case ".css":
		return Serve{Mime: "text/css; charset=utf-8"}
	case ".js":
		return Serve{Mime: "text/javascript; charset=utf-8"}
	case ".pdf":
		return Serve{Mime: "application/pdf"}
	case ".apk":
		return Serve{Mime: "application/vnd.android.package-archive"}
	}
	return Serve{Mime: "application/octet-stream"}
}
