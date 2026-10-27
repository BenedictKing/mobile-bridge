package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// trialDefaultCategories mirrors the built-in default policy category names
// (pinned by internal/jev policy tests) so the fake remote can answer with a
// full-coverage distribution.
var trialDefaultCategories = []string{
	"irreversible_destruction", "secret_exposure", "security_weakened",
	"code_from_elsewhere", "privilege_escalation", "service_disruption",
	"write_outside_allowed", "scratch_misplaced",
	"routine_work", "permission_tightened", "credential_handled_safely", "scoped_operation",
}

// trialAnswers renders one systemone reply covering every default category.
func trialAnswers() string {
	parts := make([]string, 0, len(trialDefaultCategories))
	for _, name := range trialDefaultCategories {
		parts = append(parts, fmt.Sprintf("%q:0", name))
	}
	return fmt.Sprintf(`{"answers":{"outcome":{"choice":"routine_work","probabilities":{%s}},"reversible":{"noul":0.99},"recommend":{"choice":"accept_once","confidence":0.99}}}`,
		strings.Join(parts, ","))
}

// trialHarness wires one authenticated gateway against a fake systemone
// remote that counts calls.
type trialHarness struct {
	server *Server
	token  string
	csrf   string
	remote *httptest.Server
	calls  *int32
	dir    string
}

func newTrialHarness(t *testing.T, configure func(value *jsoncompat.Object)) *trialHarness {
	t.Helper()
	var calls int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(trialAnswers()))
	}))
	t.Cleanup(remote.Close)
	dir := t.TempDir()
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Jev = jev.New(filepath.Join(dir, "jev.json"))
	value := jsoncompat.NewObject()
	value.Set("apiKey", "test-key-123456")
	value.Set("baseUrl", remote.URL)
	if configure != nil {
		configure(value)
	}
	if _, err := server.Jev.Update(value); err != nil {
		t.Fatal(err)
	}
	return &trialHarness{
		server: server,
		token:  token,
		csrf:   session.MustGet("csrf").(string),
		remote: remote,
		calls:  &calls,
		dir:    dir,
	}
}

func (h *trialHarness) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/jev/trial", strings.NewReader(body))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("X-CSRF-Token", h.csrf)
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: h.token})
	response := httptest.NewRecorder()
	h.server.ServeHTTP(response, request)
	return response
}

func TestJevTrialCommandEndpoint(t *testing.T) {
	harness := newTrialHarness(t, nil)
	// Same authentication wall as /api/jev.
	bare := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/jev/trial", strings.NewReader(`{"command":"git status"}`))
	bare.Host = "127.0.0.1:8790"
	bare.Header.Set("Origin", "http://127.0.0.1:8790")
	bareResponse := httptest.NewRecorder()
	harness.server.ServeHTTP(bareResponse, bare)
	if bareResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated trial status=%d", bareResponse.Code)
	}

	for index := 0; index < 3; index++ {
		response := harness.post(t, `{"command":"git status"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("trial %d status=%d body=%s", index, response.Code, response.Body.String())
		}
		body := response.Body.String()
		for _, want := range []string{"negative_sum", "categories", "threshold", "routine_work"} {
			if !strings.Contains(body, want) {
				t.Fatalf("trial %d body missing %s: %s", index, want, body)
			}
		}
		if strings.Contains(body, "cache_hit") {
			t.Fatalf("trial %d must never hit the session cache: %s", index, body)
		}
	}
	if count := atomic.LoadInt32(harness.calls); count != 3 {
		t.Fatalf("remote calls = %d, want 3 (trials never use the cache)", count)
	}
	// Dry-run is structural: no decision journal may appear.
	if _, err := os.Stat(filepath.Join(harness.dir, "jev-decisions.jsonl")); err == nil {
		t.Fatal("trial evaluation must not write the decision journal")
	}
}

func TestJevTrialWithoutKeyReturnsChineseError(t *testing.T) {
	harness := newTrialHarness(t, func(value *jsoncompat.Object) { value.Delete("apiKey") })
	response := harness.post(t, `{"command":"git status"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "未配置 Jev API Key，无法试评") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestJevTrialAcceptsRawMethodParams(t *testing.T) {
	var captured []byte
	var calls int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		chunk := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(chunk)
		captured = chunk
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(trialAnswers()))
	}))
	defer remote.Close()
	dir := t.TempDir()
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Jev = jev.New(filepath.Join(dir, "jev.json"))
	value := jsoncompat.NewObject()
	value.Set("apiKey", "test-key-123456")
	value.Set("baseUrl", remote.URL)
	if _, err := server.Jev.Update(value); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/jev/trial",
		strings.NewReader(`{"method":"item/commandExecution/requestApproval","params":{"command":"cargo build","cwd":"/work"}}`))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	decoded, err := jsoncompat.Unmarshal(captured)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := decoded.(*jsoncompat.Object).MustGet("state").(*jsoncompat.Object)
	if state == nil || state.MustGet("request_kind") != "item/commandExecution/requestApproval" {
		t.Fatalf("request_kind not forwarded: %s", string(captured))
	}
	if state.MustGet("command") != "cargo build" {
		t.Fatalf("command not forwarded: %s", string(captured))
	}
}

func TestJevTrialRejectsEmptyBody(t *testing.T) {
	harness := newTrialHarness(t, nil)
	cases := []struct {
		name  string
		body  string
		want  string
		count int32
	}{
		{"empty object", `{}`, "请提供试评命令或完整请求", 0},
		{"blank command", `{"command":"  "}`, "试评命令不能为空", 0},
		{"method without params", `{"method":"item/commandExecution/requestApproval"}`, "params 格式不正确", 0},
		{"method with junk params", `{"method":"item/commandExecution/requestApproval","params":"cargo build"}`, "params 格式不正确", 0},
	}
	for _, item := range cases {
		response := harness.post(t, item.body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), item.want) {
			t.Fatalf("%s: status=%d body=%s", item.name, response.Code, response.Body.String())
		}
	}
	if count := atomic.LoadInt32(harness.calls); count != 0 {
		t.Fatalf("rejected trials must not reach the remote: %d calls", count)
	}
}

func TestJevTrialIgnoresAutoApproveSwitch(t *testing.T) {
	harness := newTrialHarness(t, func(value *jsoncompat.Object) { value.Set("autoApprove", false) })
	response := harness.post(t, `{"command":"git status"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	decoded, err := jsoncompat.Unmarshal(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	assessed := decoded.(*jsoncompat.Object).MustGet("assessed")
	if assessed != true {
		t.Fatalf("trial must assess regardless of the auto-approve switch: %s", response.Body.String())
	}
}

func TestJevTrialPayloadEncodesFloatsWithoutPanic(t *testing.T) {
	harness := newTrialHarness(t, nil)
	response := harness.post(t, `{"command":"git push --force"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	decoded, err := jsoncompat.Unmarshal(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The API outlet must survive re-encoding and carry real numbers.
	if len(jsoncompat.EncodeCompact(decoded)) == 0 {
		t.Fatal("trial payload failed to re-encode")
	}
	line := decoded.(*jsoncompat.Object)
	for _, field := range []string{"negative_sum", "threshold", "reversible", "confidence"} {
		if _, isNumber := line.MustGet(field).(json.Number); !isNumber {
			t.Fatalf("%s is not a number: %s", field, response.Body.String())
		}
	}
	categories, _ := line.MustGet("categories").(*jsoncompat.Object)
	if categories == nil || categories.Len() != len(trialDefaultCategories) {
		t.Fatalf("categories = %v", line.MustGet("categories"))
	}
	for _, name := range categories.Keys() {
		if _, isNumber := categories.MustGet(name).(json.Number); !isNumber {
			t.Fatalf("category %s is not a number: %s", name, response.Body.String())
		}
	}
	if strings.Contains(response.Body.String(), "test-key") {
		t.Fatal("API key leaked into the trial payload")
	}
}
