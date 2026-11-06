package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
)

func TestCreateRejectsUnavailableExplicitCwd(t *testing.T) {
	manager := acpruntime.NewCommand(fakeACPAgent(t, t.TempDir()))
	defer manager.Close()
	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for index, cwd := range []string{filepath.Join(t.TempDir(), "missing"), file} {
		response := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions",
			`{"agent":"zcode","cwd":`+jsonText(cwd)+`,"title":"无效目录","id":"4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeee`+string(rune('0'+index))+`"}`)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "项目目录不存在或不是目录") {
			t.Fatalf("cwd=%q status=%d body=%s", cwd, response.Code, response.Body.String())
		}
	}
}
