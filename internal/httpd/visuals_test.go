package httpd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/artifacts"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
	"github.com/google/uuid"
)

// visualAgent writes workspace files and streams non-text content blocks back
// when prompted: one markdown message referencing the files, one inline image
// chunk that must be persisted to MediaDir.
func visualAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import base64,json,os,sys
DIR = "` + dir + `"
PNG = b"\x89PNG\r\n\x1a\n" + b"v" * 16
def note(update):
 print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"update":update}}),flush=True)
CWD = DIR
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new":
  CWD=(m.get("params") or {}).get("cwd",DIR)
  result={"sessionId":"km-visual"}
 elif method == "session/prompt":
  with open(os.path.join(CWD,"shot.png"),"wb") as fh: fh.write(PNG)
  with open(os.path.join(CWD,"viz.html"),"w") as fh: fh.write("<html>ok</html>")
  with open(os.path.join(CWD,"app.apk"),"wb") as fh: fh.write(b"apk-binary-content")
  note({"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"text","text":"看图 ![截图](shot.png)、[可视化](viz.html) 与 [安装包](app.apk)"}});
  note({"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"image","data":base64.b64encode(PNG).decode(),"mimeType":"image/png"}});
  result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// TestVisualResultsEndToEnd walks the whole downstream chain: markdown file
// references become servable files entries, inline image chunks persist and
// come back as desktopId attachments, and both routes actually serve bytes.
func TestVisualResultsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	manager := acpruntime.NewCommand(visualAgent(t, dir))
	manager.MediaDir = filepath.Join(dir, "agent-media")
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8791, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	server.Uploads = uploads.New(dir)

	request := func(method, target, body string, header ...string) *httptest.ResponseRecorder {
		t.Helper()
		httptestRequest := httptest.NewRequest(method, "http://127.0.0.1:8791"+target, strings.NewReader(body))
		httptestRequest.Host = "127.0.0.1:8791"
		httptestRequest.Header.Set("Origin", "http://127.0.0.1:8791")
		if len(header) == 2 {
			httptestRequest.Header.Set(header[0], header[1])
		}
		if body != "" {
			httptestRequest.Header.Set("Content-Type", "application/json")
			httptestRequest.Header.Set("X-CSRF-Token", csrf)
		}
		httptestRequest.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptestRequest)
		return response
	}

	create := request(http.MethodPost, "/api/sessions",
		`{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"视觉会话","id":"8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	send := request(http.MethodPost, "/api/sessions/km-visual/send", `{"text":"生成图片"}`)
	if send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	time.Sleep(200 * time.Millisecond)

	page := request(http.MethodGet, "/api/sessions/km-visual/timeline?limit=20", "")
	if page.Code != http.StatusOK {
		t.Fatalf("timeline status=%d body=%s", page.Code, page.Body.String())
	}
	payloadValue, payloadErr := jsoncompat.Unmarshal(page.Body.Bytes())
	payload, _ := payloadValue.(*jsoncompat.Object)
	if payloadErr != nil || payload == nil {
		t.Fatalf("timeline payload not an object: %v", payloadErr)
	}

	// files: the markdown references resolve to servable workspace artifacts.
	files, _ := payload.MustGet("files").(*jsoncompat.Array)
	var imageID, htmlID, apkID string
	for _, item := range files.Items {
		file, _ := item.(*jsoncompat.Object)
		if file == nil {
			continue
		}
		if file.MustGet("image") == true {
			imageID, _ = file.MustGet("id").(string)
		} else if reference, _ := file.MustGet("reference").(string); reference == "viz.html" {
			htmlID, _ = file.MustGet("id").(string)
		} else if reference, _ := file.MustGet("reference").(string); reference == "app.apk" {
			apkID, _ = file.MustGet("id").(string)
			if file.MustGet("size") == nil || file.MustGet("modifiedAt") == nil {
				t.Fatalf("apk metadata missing: %s", page.Body.String())
			}
		}
	}
	if imageID == "" || htmlID == "" || apkID == "" {
		t.Fatalf("files incomplete (image=%q html=%q apk=%q): %s", imageID, htmlID, apkID, page.Body.String())
	}

	// The persisted inline image comes back as a desktopId attachment, and
	// the private path never ships.
	rows, _ := payload.MustGet("rows").(*jsoncompat.Array)
	var desktopID string
	for _, item := range rows.Items {
		row, _ := item.(*jsoncompat.Object)
		if row == nil {
			continue
		}
		attachments, _ := row.MustGet("attachments").(*jsoncompat.Array)
		if attachments == nil {
			continue
		}
		for _, attachmentValue := range attachments.Items {
			attachment, _ := attachmentValue.(*jsoncompat.Object)
			if attachment == nil {
				continue
			}
			if _, has := attachment.Get("path"); has {
				t.Fatalf("attachment leaked a local path: %s", attachment)
			}
			desktopID, _ = attachment.MustGet("desktopId").(string)
		}
	}
	if desktopID == "" {
		t.Fatalf("no desktopId attachment in timeline: %s", page.Body.String())
	}

	media := request(http.MethodGet, "/api/sessions/km-visual/desktop-images/"+desktopID, "")
	if media.Code != http.StatusOK || media.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("desktop image status=%d type=%s", media.Code, media.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(media.Body.String(), "\x89PNG") {
		t.Fatal("desktop image bytes are not PNG")
	}

	// A made-up id never resolves, even with a live session.
	missing := request(http.MethodGet, "/api/sessions/km-visual/desktop-images/"+strings.Repeat("ab", 32), "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown desktop id status=%d", missing.Code)
	}

	html := request(http.MethodGet, "/api/sessions/km-visual/files/"+htmlID, "")
	if html.Code != http.StatusOK ||
		html.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		html.Header().Get("Content-Security-Policy") != "sandbox allow-scripts; frame-ancestors 'self'" {
		t.Fatalf("html artifact status=%d headers=%v", html.Code, html.Header())
	}
	// Sandboxed documents frame only from gateway pages: XFO must yield to the
	// response's own frame-ancestors, or the chat's iframe preview is blocked.
	if html.Header().Get("X-Frame-Options") != "" {
		t.Fatalf("sandboxed html kept X-Frame-Options: %v", html.Header())
	}
	shot := request(http.MethodGet, "/api/sessions/km-visual/files/"+imageID, "")
	if shot.Code != http.StatusOK || shot.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image artifact status=%d type=%s", shot.Code, shot.Header().Get("Content-Type"))
	}
	// Non-sandboxed media keeps the blanket framing ban.
	if shot.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("image artifact lost X-Frame-Options: %v", shot.Header())
	}
	apk := request(http.MethodGet, "/api/sessions/km-visual/files/"+apkID, "")
	if apk.Code != http.StatusOK || apk.Body.String() != "apk-binary-content" ||
		apk.Header().Get("Content-Type") != "application/vnd.android.package-archive" ||
		apk.Header().Get("Content-Disposition") != `attachment; filename="app.apk"; filename*=UTF-8''app.apk` {
		t.Fatalf("apk download status=%d headers=%v body=%q", apk.Code, apk.Header(), apk.Body.String())
	}
	partial := request(http.MethodGet, "/api/sessions/km-visual/files/"+apkID, "", "Range", "bytes=0-3")
	if partial.Code != http.StatusPartialContent || partial.Body.String() != "apk-" ||
		partial.Header().Get("Content-Range") != "bytes 0-3/18" {
		t.Fatalf("apk range status=%d headers=%v body=%q", partial.Code, partial.Header(), partial.Body.String())
	}
}

// Uploaded images referenced by agent output keep their upload preview ids
// (thumbs included); agent-produced paths get desktopIds instead.
func TestAnnotateAttachmentsUploadLedger(t *testing.T) {
	dir := t.TempDir()
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8792, "instance")
	server.Uploads = uploads.New(dir)

	png := []byte("\x89PNG\r\n\x1a\nuploaded")
	id := uuid.NewString()
	upload, err := server.Uploads.Put("thread-1", id, "photo.png", png)
	if err != nil {
		t.Fatal(err)
	}
	_ = upload
	localPath := filepath.Join(dir, "uploads", "thread-1", id, "internal", "original.png")

	agentPath := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(agentPath, png, 0o600); err != nil {
		t.Fatal(err)
	}

	row := func(path string) *jsoncompat.Object {
		attachment := jsoncompat.NewObject()
		attachment.Set("type", "localImage")
		attachment.Set("path", path)
		message := jsoncompat.NewObject()
		message.Set("role", "user")
		list := &jsoncompat.Array{Items: []any{attachment}}
		message.Set("attachments", list)
		return message
	}
	rows := &jsoncompat.Array{Items: []any{row(localPath), row(agentPath)}}
	server.annotateAttachments("thread-1", rows)

	uploaded, _ := rows.Items[0].(*jsoncompat.Object)
	uploadAttachment := firstAttachment(t, uploaded)
	if uploadAttachment.MustGet("uploadId") != id || uploadAttachment.MustGet("mime") != "image/png" {
		t.Fatalf("upload annotation wrong: %s", uploadAttachment)
	}
	if _, has := uploadAttachment.Get("path"); has {
		t.Fatal("upload attachment leaked its path")
	}

	agent, _ := rows.Items[1].(*jsoncompat.Object)
	agentAttachment := firstAttachment(t, agent)
	desktopID, _ := agentAttachment.MustGet("desktopId").(string)
	if desktopID == "" {
		t.Fatalf("agent attachment missing desktopId: %s", agentAttachment)
	}
	if name, _ := agentAttachment.MustGet("name").(string); name != "shot.png" {
		t.Fatalf("agent attachment name=%q", name)
	}

	// The registered id serves the real bytes; another thread cannot see it.
	if path, ok := server.desktopImage("thread-1", desktopID); !ok || path != artifacts.Identity(agentPath) {
		t.Fatalf("registry lookup: %q %v", path, ok)
	}
	if _, ok := server.desktopImage("thread-2", desktopID); ok {
		t.Fatal("desktop image leaked across threads")
	}
}

func TestContentDispositionIncludesASCIIFallback(t *testing.T) {
	tests := []struct {
		disposition string
		name        string
		want        string
	}{
		{"attachment", "app-debug.apk", `attachment; filename="app-debug.apk"; filename*=UTF-8''app-debug.apk`},
		{"inline", "截图.png", `inline; filename="__.png"; filename*=UTF-8''%E6%88%AA%E5%9B%BE.png`},
		{"attachment", "a\"b\\c/d;e\n.apk", `attachment; filename="a_b_c_d_e_.apk"; filename*=UTF-8''a%22b%5Cc%2Fd%3Be%0A.apk`},
		{"attachment", "", `attachment; filename="download"; filename*=UTF-8''`},
	}
	for _, test := range tests {
		if got := contentDisposition(test.disposition, test.name); got != test.want {
			t.Errorf("contentDisposition(%q, %q) = %q, want %q", test.disposition, test.name, got, test.want)
		}
	}
}

func firstAttachment(t *testing.T, row *jsoncompat.Object) *jsoncompat.Object {
	t.Helper()
	attachments, _ := row.MustGet("attachments").(*jsoncompat.Array)
	if attachments == nil || len(attachments.Items) == 0 {
		t.Fatal("no attachments on row")
	}
	attachment, _ := attachments.Items[0].(*jsoncompat.Object)
	if attachment == nil {
		t.Fatal("attachment not an object")
	}
	return attachment
}
