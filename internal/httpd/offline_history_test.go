package httpd

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/store"
)

func TestOfflineHistoryRequiresPairingAndReadsOnlySavedPrefix(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	home := f.dir
	id := "a98a4718-1358-40c8-98f0-61f2fac41022"
	route := "/api/mobile/offline-history?id=" + id + "&agent=codex&host=local"
	const mobile = "BridgeMobile/0.1-iOS"
	token, _, err := f.auth.NewSession("127.0.0.1", mobile, false)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, target, ua, credential string) int {
		response := f.serve(offlineHistoryRequest(f, method, target, ua, credential))
		return response.Code
	}
	if code := request(http.MethodGet, route, mobile, ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", code)
	}
	if code := request(http.MethodGet, route, mobile, token); code != http.StatusForbidden {
		t.Fatalf("unpaired status=%d", code)
	}
	if code := request(http.MethodGet, "/api/auth", mobile, token); code != http.StatusOK {
		t.Fatalf("native auth status=%d", code)
	}
	if code := request(http.MethodGet, route, "Desktop Browser", token); code != http.StatusForbidden {
		t.Fatalf("browser status=%d", code)
	}
	for _, foreign := range []string{strings.Replace(route, "host=local", "host=ssh-other", 1), strings.Replace(route, "agent=codex", "agent=claude", 1)} {
		if code := request(http.MethodGet, foreign, mobile, token); code != http.StatusBadRequest {
			t.Fatalf("unsupported source status=%d", code)
		}
	}
	if code := request(http.MethodPost, route, mobile, token); code != http.StatusMethodNotAllowed {
		t.Fatalf("write status=%d", code)
	}

	rollout := filepath.Join(home, "sessions", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, originator TEXT, source TEXT, rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO threads VALUES (?, '离线测试', '/synthetic', 1, 0, 'Codex Desktop', 'desktop', ?)`, id, rollout); err != nil {
		t.Fatal(err)
	}
	f.server.Store = store.NewSessionStore(home)
	longText := strings.Repeat("中文🚀", 9000)
	meta := `{"type":"session_meta","payload":{"id":"` + id + `"}}` + "\n"
	saved := meta + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"first"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"最早问题"}]}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call1","arguments":"echo test"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call1","output":"tool-result"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + longText + `"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"first"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"current"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"not-yet-saved"}]}}
{"type":"response_item"`
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(rollout, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(saved)
	response := f.serve(offlineHistoryRequest(f, http.MethodGet, route, mobile, token))
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", response.Code, response.Body.String())
	}
	var result offlineHistory
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != id || result.DeviceID != f.auth.DeviceID() || result.Through != "first" || result.Title != "离线测试" {
		t.Fatalf("unexpected snapshot identity: %s", response.Body.Bytes()[:min(300, response.Body.Len())])
	}
	found := map[string]bool{}
	for _, row := range result.Rows {
		if row.TurnID == "current" || strings.Contains(row.Text, "not-yet-saved") || row.Key == "" || row.Version == "" {
			t.Fatalf("invalid snapshot row: %+v", row)
		}
		found["full"] = found["full"] || row.Text == longText
		found["oldest"] = found["oldest"] || row.Text == "最早问题"
		found["tool"] = found["tool"] || strings.Contains(row.Text, "tool-result")
	}
	if !found["full"] || !found["oldest"] || !found["tool"] {
		t.Fatalf("missing text or process: %v", found)
	}
	digest := sha256.Sum256(response.Body.Bytes())
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Bridge-Offline-SHA256") != hex.EncodeToString(digest[:]) {
		t.Fatalf("invalid response headers: %v", response.Header())
	}
	for name, variant := range map[string]string{
		"bad identity":   strings.Replace(saved, id, "some-other-thread", 1),
		"bad completion": strings.Replace(saved, `"task_complete","turn_id":"first"`, `"task_complete","turn_id":"other"`, 1),
		"damaged middle": strings.Replace(saved, `{"type":"response_item","payload":{"type":"function_call"`, `broken {"type":"response_item","payload":{"type":"function_call"`, 1),
		"unfinished":     meta + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"first"}}`,
	} {
		write(variant)
		if code := request(http.MethodGet, route, mobile, token); code != http.StatusConflict {
			t.Fatalf("%s status=%d", name, code)
		}
	}
	write(strings.Repeat(" ", 16<<20+1))
	if code := request(http.MethodGet, route, mobile, token); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized source status=%d", code)
	}
	f.auth.Logout(token)
	if code := request(http.MethodGet, route, mobile, token); code != http.StatusUnauthorized {
		t.Fatalf("revoked binding status=%d", code)
	}
}

func offlineHistoryRequest(f *mobileAPIFixture, method, route, ua, token string) *http.Request {
	req := f.build(method, route, "", false)
	req.Header.Set("User-Agent", ua)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: f.auth.CookieName(), Value: token})
	}
	return req
}
