// Visual results plumbing: the timeline payloads the phone renders carry two
// preview channels this file fills — `files` (workspace/visualization
// artifacts referenced by markdown, served by /files/{sha256}) and
// attachment preview ids (uploadId from the phone's own upload ledger,
// desktopId for anything else, served by /desktop-images/{sha256}). It is
// the Go port of Python's files.py + _annotate_upload_attachments +
// desktop_image_preview trio.
package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/BenedictKing/mobile-bridge/internal/artifacts"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

var desktopImageRoute = regexp.MustCompile(
	`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/desktop-images/([a-f0-9]{64})$`)

// artifactMemo caches one thread's collected artifacts per projection
// sequence, so the 200ms SSE loop only re-walks the view on real changes.
// Published entries are immutable; readers must not mutate them.
type artifactMemo struct {
	sequence string
	files    map[string]artifacts.Entry
}

// enrichTimeline attaches the chat's visual artifacts to a timeline payload:
// the files array for markdown rendering, plus preview ids on row
// attachments (path is stripped — private filesystem layout never crosses
// the API boundary). The payload is a projection clone, safe to mutate.
func (s *Server) enrichTimeline(threadID string, view *jsoncompat.Object, result *jsoncompat.Object) *jsoncompat.Object {
	if result == nil {
		return nil
	}
	cwd := viewCwd(view)
	files := s.artifactFiles(threadID, view, cwd)
	result.Set("files", artifacts.FilesPayload(files))
	if rows, ok := result.MustGet("rows").(*jsoncompat.Array); ok {
		s.annotateAttachments(threadID, rows)
	}
	return result
}

func viewCwd(view *jsoncompat.Object) string {
	cwd, _ := view.MustGet("cwd").(string)
	return cwd
}

func (s *Server) artifactFiles(threadID string, view *jsoncompat.Object, cwd string) map[string]artifacts.Entry {
	sequence := ""
	if number, ok := view.MustGet("sequence").(json.Number); ok {
		sequence = number.String()
	}
	s.artifactMu.Lock()
	memo := s.artifactCache[threadID]
	if memo != nil && memo.sequence == sequence {
		files := memo.files
		s.artifactMu.Unlock()
		return files
	}
	s.artifactMu.Unlock()
	files := artifacts.Collect(view, cwd)
	s.artifactMu.Lock()
	s.artifactCache[threadID] = &artifactMemo{sequence: sequence, files: files}
	s.artifactMu.Unlock()
	return files
}

// annotateAttachments adds preview ids to one payload's row attachments:
// images the phone uploaded get their uploadId back; anything else the agent
// produced gets a desktopId keyed by the path hash. Either way the raw path
// is removed from what ships to the client (Python _annotate_upload_
// attachments).
func (s *Server) annotateAttachments(threadID string, rows *jsoncompat.Array) {
	paths := map[string]bool{}
	eachImageAttachment(rows, func(attachment *jsoncompat.Object) {
		if path, _ := attachment.MustGet("path").(string); path != "" {
			paths[artifacts.Identity(path)] = true
		}
	})
	var uploadsByPath map[string]*jsoncompat.Object
	if len(paths) > 0 && s.Uploads != nil {
		uploadsByPath = s.Uploads.ByPath(threadID, paths)
	}
	eachImageAttachment(rows, func(attachment *jsoncompat.Object) {
		path, _ := attachment.MustGet("path").(string)
		if path == "" {
			return
		}
		identity := artifacts.Identity(path)
		if upload := uploadsByPath[identity]; upload != nil {
			if id, _ := upload.MustGet("id").(string); id != "" {
				attachment.Set("uploadId", id)
			}
			if image, _ := upload.MustGet("image").(string); image != "" {
				attachment.Set("mime", image)
			}
			if thumb, ok := upload.Get("thumb"); ok && thumb != nil {
				attachment.Set("thumb", thumb)
			}
			if name, _ := upload.MustGet("name").(string); name != "" {
				attachment.Set("name", name)
			}
		} else {
			attachment.Set("desktopId", artifacts.ID(path))
			s.desktopRegister(threadID, artifacts.ID(path), identity)
		}
		if name, _ := attachment.MustGet("name").(string); name == "" {
			attachment.Set("name", filepath.Base(path))
		}
		attachment.Delete("path")
	})
}

func eachImageAttachment(rows *jsoncompat.Array, visit func(attachment *jsoncompat.Object)) {
	for _, rowValue := range rows.Items {
		row, _ := rowValue.(*jsoncompat.Object)
		if row == nil {
			continue
		}
		attachments, _ := row.MustGet("attachments").(*jsoncompat.Array)
		if attachments == nil {
			continue
		}
		for _, item := range attachments.Items {
			attachment, _ := item.(*jsoncompat.Object)
			if attachment == nil {
				continue
			}
			attachmentType, _ := attachment.MustGet("type").(string)
			if attachmentType != "localImage" && attachmentType != "image" {
				continue
			}
			visit(attachment)
		}
	}
}

func (s *Server) desktopRegister(threadID, id, path string) {
	s.desktopMu.Lock()
	defer s.desktopMu.Unlock()
	if s.desktopPaths[threadID] == nil {
		s.desktopPaths[threadID] = map[string]string{}
	}
	s.desktopPaths[threadID][id] = path
}

// desktopImage resolves one desktop image id: the annotation registry first,
// then a fresh walk of the chat's own state (cold start after a restart,
// mirroring Python's desktop_image_preview).
func (s *Server) desktopImage(threadID, id string) (string, bool) {
	s.desktopMu.Lock()
	var path string
	if registry := s.desktopPaths[threadID]; registry != nil {
		path = registry[id]
	}
	s.desktopMu.Unlock()
	if path != "" {
		return path, true
	}
	if view := s.bestEffortView(threadID); view != nil {
		for _, candidate := range artifacts.AttachmentPaths(view) {
			if artifacts.ID(candidate) == id {
				return candidate, true
			}
		}
	}
	return "", false
}

// artifactLookup resolves one workspace artifact id for the files route.
// The timeline memo covers the hot path; a cold-start direct hit re-collects
// from the chat's current state.
func (s *Server) artifactLookup(threadID, id string) (artifacts.Entry, bool) {
	s.artifactMu.Lock()
	memo := s.artifactCache[threadID]
	s.artifactMu.Unlock()
	if memo != nil {
		if entry, ok := memo.files[id]; ok {
			return entry, true
		}
	}
	if view := s.bestEffortView(threadID); view != nil {
		if entry, ok := artifacts.Collect(view, viewCwd(view))[id]; ok {
			return entry, true
		}
	}
	return artifacts.Entry{}, false
}

// bestEffortView returns the thread's normalized view without spawning any
// agent process: live ACP sessions, the codex follower snapshot, then saved
// history.
func (s *Server) bestEffortView(threadID string) *jsoncompat.Object {
	agents := s.acpAgentSet()
	kinds := make([]string, 0, len(agents))
	for kind := range agents {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		manager := agents[kind]
		if manager == nil {
			continue
		}
		if session := manager.Get(threadID); session != nil {
			return s.acpViewState(kind, manager, session)
		}
	}
	if s.Codex != nil {
		if state, _, _, _ := s.Codex.Snapshot(threadID); state != nil {
			return model.NormalizeState(state, true)
		}
	}
	if s.Store != nil {
		if history, err := s.Store.HistoryFor(threadID, nil); err == nil {
			return buildHistoryView(history)
		}
	}
	return nil
}

// buildHistoryView shapes saved codex history into the normalized view the
// timeline and artifact collectors consume.
func buildHistoryView(history *jsoncompat.Object) *jsoncompat.Object {
	state := jsoncompat.NewObject()
	for _, key := range []string{"id", "title", "cwd", "latestModel", "modelProvider", "turns"} {
		if value, ok := history.Get(key); ok {
			state.Set(key, value)
		}
	}
	state.Set("turnsPagination", history.MustGet("turnsPagination"))
	var sequence any = jsoncompat.Num(1)
	if value, ok := history.Get("updated_at_ms"); ok {
		sequence = value
	} else if value, ok := history.Get("updated_at"); ok {
		sequence = value
	}
	state.Set("sequence", sequence)
	view := model.NormalizeState(state, false)
	// Saved rollouts carry no structured fileChange/commandExecution items
	// today, so this is a no-op for current history — kept here so saved and
	// live projections of the same thread annotate identically.
	annotateViewChanges(view, model.OrderedTurns(state))
	view.Set("sequence", sequence)
	return view
}

// serveDesktopImage serves one agent-side image by path hash. Only files the
// chat itself referenced are reachable, and the magic bytes must really be a
// raster image — an agent cannot turn this route into a general file server.
func (h *handler) serveDesktopImage(threadID, id string) {
	path, ok := h.server.desktopImage(threadID, id)
	if !ok {
		h.fail(http.StatusNotFound, errorBody("桌面图片不存在或已不可用"))
		return
	}
	data, mime, ok := readServableFile(path)
	if !ok || mime == "" {
		h.fail(http.StatusNotFound, errorBody("桌面图片不存在或已不可用"))
		return
	}
	name := filepath.Base(path)
	h.writeMedia(http.StatusOK, data, mime, name, "")
}

// serveWorkspaceArtifact serves one workspace/visualization file the chat
// referenced through the files route — raster images inline, HTML previews
// sandboxed, everything else a download.
func (h *handler) serveWorkspaceArtifact(threadID, id string) {
	entry, ok := h.server.artifactLookup(threadID, id)
	if !ok {
		h.fail(http.StatusNotFound, errorBody("文件不存在或不属于此聊天"))
		return
	}
	data, _, ok := readServableFile(entry.Path)
	if !ok {
		h.fail(http.StatusNotFound, errorBody("文件已不可用"))
		return
	}
	serve := artifacts.ServeType(entry.Path, data)
	// octet-stream downloads regardless of disposition; everything viewable
	// stays inline.
	h.writeMedia(http.StatusOK, data, serve.Mime, entry.Name, serve.Sandbox)
}

// readServableFile stats and reads one servable file with the shared size
// bound. The returned mime is empty unless the content sniffs as a raster
// image.
func readServableFile(path string) ([]byte, string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > artifacts.MaxFile {
		return nil, "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", false
	}
	return data, artifacts.SniffImage(data), true
}

// writeMedia emits one media response with revalidation caching. Sandbox, when
// non-empty, becomes the Content-Security-Policy so agent-authored documents
// render their scripts without inheriting the gateway's origin — those
// responses declare frame-ancestors 'self' instead of X-Frame-Options, whose
// DENY would veto the chat's own same-origin iframe preview.
func (h *handler) writeMedia(status int, data []byte, mime, name, sandbox string) {
	etag := `"` + contentHash(data) + `"`
	header := h.writer.Header()
	header.Set("Cache-Control", "private, no-cache")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	if sandbox == "" {
		header.Set("X-Frame-Options", "DENY")
	}
	if sandbox != "" {
		header.Set("Content-Security-Policy", sandbox)
	}
	header.Set("Content-Type", mime)
	header.Set("Content-Length", strconv.Itoa(len(data)))
	header.Set("ETag", etag)
	header.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(name))
	if status != http.StatusNotModified && h.request.Header.Get("If-None-Match") == etag {
		status = http.StatusNotModified
		data = nil
		header.Del("Content-Length")
		header.Del("Content-Disposition")
	}
	h.writer.WriteHeader(status)
	_, _ = h.writer.Write(data)
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
