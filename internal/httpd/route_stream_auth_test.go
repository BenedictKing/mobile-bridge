package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// Revocation after the first data frame checks both admission and the next
// stream auth check, without waiting for a heartbeat or a live agent.
type revokeOnDataWriter struct {
	*httptest.ResponseRecorder
	revoke func()
	data   bool
}

func (w *revokeOnDataWriter) Write(value []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(value)
	text := string(value)
	if !w.data && (strings.Contains(text, "event: state\n") ||
		strings.Contains(text, "event: reset\n") || strings.Contains(text, "event: changes\n")) {
		w.data = true
		w.revoke()
	}
	return n, err
}

func TestStreamsAcceptRouteCookieAndHonorRevocation(t *testing.T) {
	for _, mode := range []string{"snapshot", "incremental", "team"} {
		t.Run(mode, func(t *testing.T) {
			f := newMobileAPIFixture(t, "none")
			target := "http://127.0.0.1:8790"
			credential := routeCredential(f.token, target)
			bindings := jsoncompat.NewObject()
			bindings.Set(target, routeDigest(credential))
			f.auth.MutateSessionByKey(auth.SessionKey(f.token), func(session *jsoncompat.Object) bool {
				session.Set("routeBindings", bindings)
				return true
			})
			request := f.build(http.MethodGet, "/events", "", false)
			request.AddCookie(&http.Cookie{Name: f.auth.CookieName(), Value: credential})
			writer := &revokeOnDataWriter{ResponseRecorder: httptest.NewRecorder(),
				revoke: func() { f.auth.Logout(f.token) }}
			h := &handler{server: f.server, writer: writer, request: request}
			if mode == "team" {
				h.teamStreamRunEvents(team.NewEventFeed("route-test", nil))
			} else {
				query := map[string][]string{}
				if mode == "incremental" {
					query["incremental"] = []string{"1"}
				}
				h.stream(NewScriptedBridge(), "route-test", query)
			}
			if !writer.data {
				t.Fatalf("valid route cookie logged out before data: %s", writer.Body.String())
			}
			if !strings.Contains(writer.Body.String(), "event: logout\n") {
				t.Fatalf("revoked session kept streaming: %s", writer.Body.String())
			}
		})
	}
}

func TestRouteAuthDoesNotExpireInstalledCookie(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	target := "http://127.0.0.1:8790"
	credential := routeCredential(f.token, target)
	bindings := jsoncompat.NewObject()
	bindings.Set(target, routeDigest(credential))
	f.auth.MutateSessionByKey(auth.SessionKey(f.token), func(session *jsoncompat.Object) bool {
		session.Set("routeBindings", bindings)
		return true
	})
	request := f.build(http.MethodGet, "/api/auth", "", false)
	request.AddCookie(&http.Cookie{Name: f.auth.CookieName(), Value: credential})
	response := f.serve(request)
	if decodeMobileJSON(t, response)["authenticated"] != true {
		t.Fatal("route cookie must authenticate")
	}
	if cookies := response.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("route authentication must preserve the installed cookie: %v", cookies)
	}
	root := f.request(http.MethodGet, "/api/auth", "")
	if root.Header().Get("Set-Cookie") == "" {
		t.Fatal("root login renewal must remain enabled")
	}
}
