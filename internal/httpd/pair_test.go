package httpd

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/pairing"
)

func TestPairExchangeCreatesAuthenticatedSession(t *testing.T) {
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance")
	server.Hosts["example.com"] = true
	server.Origins["https://example.com"] = true
	server.Pairing = pairing.New([]string{"https://example.com"})
	grant, err := server.Pairing.Control(jsonObject(t, `{"action":"create","url":"https://example.com"}`), float64(time.Now().Unix()), func() string {
		return strings.Repeat("t", 43)
	}, func() string { return "grant-1" })
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://example.com/api/pair", strings.NewReader(`{"token":"`+strings.Repeat("t", 43)+`"}`))
	request.Host = "example.com"
	request.Header.Set("Origin", "https://example.com")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Set-Cookie") == "" || !strings.Contains(response.Body.String(), `"csrf"`) {
		t.Fatalf("missing session response: headers=%v body=%s", response.Header(), response.Body.String())
	}
	if grant.MustGet("state") != "active" {
		t.Fatal("control result unexpectedly changed")
	}
}

type fsys struct{}

func (fsys) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func jsonObject(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*jsoncompat.Object)
}
