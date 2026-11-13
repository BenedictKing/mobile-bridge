package httpd

import (
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var testNativeLAN = flag.Bool("native-lan", false, "compile Java/Swift clients and test against an isolated Go gateway")

// Opt in on a macOS development host with Swift and Java installed:
// go test ./internal/httpd -run TestNativeLANClients -args -native-lan
func TestNativeLANClients(t *testing.T) {
	if !*testNativeLAN {
		t.Skip("requires native toolchains and a private interface; use -args -native-lan")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var host string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, _ := net.ParseCIDR(address.String())
			if ip.To4() != nil && ip.IsPrivate() {
				host = ip.String()
				break
			}
		}
		if host != "" {
			break
		}
	}
	if host == "" {
		t.Fatal("no private IPv4 interface for isolated LAN integration test")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	f := newMobileAPIFixture(t, "none")
	target := "http://" + listener.Addr().String()
	f.server.Hosts[listener.Addr().String()] = true
	f.server.Origins[target] = true
	token, _, err := f.auth.NewSession(host, "BridgeMobile/0.1-iOS", false)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	name := f.auth.CookieName()
	var mu sync.Mutex
	var requests []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path+" "+r.Header.Get("Cookie"))
		mu.Unlock()
		f.server.ServeHTTP(w, r)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	work := t.TempDir()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	run := func(command string, args ...string) {
		t.Helper()
		output, err := exec.Command(command, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", command, err, output)
		}
		if len(output) > 0 {
			t.Log(string(output))
		}
	}
	run("javac", "-d", work, filepath.Join(root, "android/app/src/main/java/com/mobilebridge/app/RouteCredentials.java"), "testdata/LANContractTest.java")
	run("java", "-cp", work, "com.mobilebridge.app.LANContractTest", target, f.auth.DeviceID(), token)
	binary := filepath.Join(work, "lan-native")
	run("swiftc", "-parse-as-library", "-module-cache-path", filepath.Join(work, "swift-cache"),
		filepath.Join(root, "mobile/ios/BridgePreview/GatewayURL.swift"),
		filepath.Join(root, "mobile/ios/BridgePreview/ConnectionDiscovery.swift"), "testdata/lan-native.swift", "-o", binary)
	run(binary, target, f.auth.DeviceID(), token, name)
	mu.Lock()
	defer mu.Unlock()
	rootAuth, routeAuth := 0, 0
	for _, request := range requests {
		switch {
		case strings.HasPrefix(request, "/api/connection/prove "):
			if request != "/api/connection/prove " {
				t.Fatal("proof request leaked a Cookie")
			}
		case request == "/api/auth "+name+"="+token:
			rootAuth++
		case strings.HasPrefix(request, "/api/auth "+name+"=cmbroute1_"):
			routeAuth++
		default:
			t.Fatalf("unexpected native request: %s", strings.Split(request, " ")[0])
		}
	}
	if rootAuth != 1 || routeAuth != 1 {
		t.Fatalf("root auth=%d route auth=%d; want one each", rootAuth, routeAuth)
	}
}
