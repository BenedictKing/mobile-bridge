package httpd

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/claude"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
)

func TestClaudeBase64HistoryImagesServeThroughTimelineAttachmentIDs(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(imageBytes.Bytes())
	const sessionID = "claude-image-session"
	content := `{"type":"user","cwd":"/work","message":{"content":[{"type":"text","text":"看看图片"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}}]}}`
	if err := os.WriteFile(filepath.Join(project, sessionID+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := claude.TranscriptReader(root, filepath.Join(root, "media"))(sessionID)
	if err != nil || snapshot == nil {
		t.Fatalf("native snapshot: %v", err)
	}
	view := acpview.NewView(sessionID, nil, "/work", func() float64 { return 0 })
	view.ApplyNative(snapshot)
	state := view.Snapshot()
	state.Set("sequence", jsoncompat.Num(1))
	projection := timeline.New(func() string { return "image-epoch" })
	projection.Update(state)
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8791, "instance")
	server.Uploads = uploads.New(root)
	page := server.enrichTimeline(sessionID, state, projection.Page(20, ""))
	row := page.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	attachment := firstAttachment(t, row)
	id, _ := attachment.MustGet("desktopId").(string)
	if id == "" || row.MustGet("text") != "看看图片" || attachment.MustGet("path") != nil || strings.Contains(string(jsoncompat.EncodeCompact(page)), encoded) {
		t.Fatal("图片必须以附件 ID 投影，不能包含原始路径或 Base64")
	}
	token, _, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	request := func(session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8791/api/sessions/"+session+"/desktop-images/"+id, nil)
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	response := request(sessionID)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/png" || !bytes.Equal(response.Body.Bytes(), imageBytes.Bytes()) {
		t.Fatalf("图片预览未正确返回: %d %s", response.Code, response.Header().Get("Content-Type"))
	}
	if response := request("another-session"); response.Code != http.StatusNotFound {
		t.Fatalf("图片附件不能跨会话访问: %d", response.Code)
	}
}
