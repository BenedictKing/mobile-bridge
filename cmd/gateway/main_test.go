package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestFreshWorkerSnapshotSeedsConfigWithoutReturningPasswordHash(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "gateway")
	snapshot := workerSnapshot(dataDir)
	authValue, ok := snapshot.MustGet("auth").(*jsoncompat.Object)
	if !ok || authValue.MustGet("username") != "admin" || authValue.MustGet("mode") != "password" {
		t.Fatalf("missing public auth snapshot: %s", jsoncompat.EncodePythonDefault(snapshot))
	}
	if _, exists := authValue.Get("hash"); exists {
		t.Fatal("password hash leaked in desktop snapshot")
	}
	if _, exists := authValue.Get("salt"); exists {
		t.Fatal("password salt leaked in desktop snapshot")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "首次登录.txt")); err != nil {
		t.Fatalf("first-run credential file missing: %v", err)
	}
}

func TestGatewayListenAddressesHonorsLanSelection(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "desktop.json"), []byte(`{"lan":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gatewayListenAddresses(dataDir); len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("lan disabled addresses=%v", got)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "desktop.json"), []byte(`{"lan":true,"localAccess":true,"lanAddresses":["192.0.2.10","192.0.2.10"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := gatewayListenAddresses(dataDir)
	if len(got) != 2 || got[0] != "127.0.0.1" || got[1] != "192.0.2.10" {
		t.Fatalf("selected addresses=%v", got)
	}
}

func TestClassifyArgsHelpVariantsAndUnknownRejected(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"serve", "--help"}, {"snapshot", "-h"}} {
		help, action, _, err := classifyArgs(args)
		if !help || err != nil || action != "" {
			t.Fatalf("args=%v help=%v action=%q err=%v", args, help, action, err)
		}
	}
	// Bare invocation keeps the serve default.
	if help, action, _, err := classifyArgs(nil); help || action != "" || err != nil {
		t.Fatalf("bare help=%v action=%q err=%v", help, action, err)
	}
	help, action, rest, err := classifyArgs([]string{"serve", "--data-dir", "X"})
	if help || err != nil || action != "serve" || len(rest) != 2 {
		t.Fatalf("serve help=%v action=%q rest=%v err=%v", help, action, rest, err)
	}
	for _, args := range [][]string{{"frobnicate"}, {"--data-dir", "X"}} {
		if _, _, _, err := classifyArgs(args); err == nil {
			t.Fatalf("args=%v must be rejected", args)
		}
	}
}

func TestApplyServeFlagsRejectsUnknownAndMissingValue(t *testing.T) {
	if err := applyServeFlags([]string{"--port", "9000"}); err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}
	if err := applyServeFlags([]string{"--wat", "1"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if err := applyServeFlags([]string{"--port"}); err == nil {
		t.Fatal("missing value accepted")
	}
}

func TestTrustedOriginsFromConfigAndDesktopConnections(t *testing.T) {
	dataDir := t.TempDir()
	config := `{"origins":["https://stored.example"],"connections":[` +
		`{"id":"a","enabled":true,"accessMode":"cloudflare","publicUrl":"https://a.example"},` +
		`{"id":"b","enabled":false,"accessMode":"cloudflare","publicUrl":"https://b.example"},` +
		`{"id":"c","enabled":true,"accessMode":"nas","publicUrl":"https://c.example"},` +
		`{"id":"d","enabled":true,"accessMode":"quick","publicUrl":"https://d.example"}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	managed := jsoncompat.NewObject()
	managed.Set("id", "e")
	managed.Set("enabled", true)
	managed.Set("accessMode", "server")
	managed.Set("publicUrl", "https://e.example")
	connections := &jsoncompat.Array{}
	connections.Items = append(connections.Items, managed)
	preferences := jsoncompat.NewObject()
	preferences.Set("connections", connections)

	got := trustedOrigins(dataDir, preferences)
	want := []string{"https://stored.example", "https://a.example", "https://c.example", "https://e.example"}
	if len(got) != len(want) {
		t.Fatalf("origins=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("origins=%v want=%v", got, want)
		}
	}
}

func TestTeamReconcileIntervalFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"未设置回默认", "", 60 * time.Second},
		{"正整数秒", "5", 5 * time.Second},
		{"非法值回默认", "abc", 60 * time.Second},
		{"零回默认", "0", 60 * time.Second},
		{"负数回默认", "-3", 60 * time.Second},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "GATEWAY_TEAM_RECONCILE_SECONDS" {
					return testCase.raw
				}
				return ""
			}
			if got := teamReconcileIntervalFromEnv(getenv); got != testCase.want {
				t.Fatalf("间隔应为 %v，实际 %v", testCase.want, got)
			}
		})
	}
}

func writeDirectTestCertificate(t *testing.T, dataDir string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lan.bridge.example"},
		DNSNames: []string{"lan.bridge.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dataDir, "direct.crt"), cert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "direct.key"), privateKey, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirectSettings(t *testing.T) {
	dataDir := t.TempDir()
	writeDirectTestCertificate(t, dataDir)
	config := `{"direct":{"publicUrl":"https://bridge.example","url":"https://lan.bridge.example:8788/","certFile":"direct.crt","keyFile":"direct.key"}}`
	if err := os.WriteFile(filepath.Join(dataDir, "desktop.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := loadDirectSettings(dataDir, desktopPreferences(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if settings == nil || settings.publicOrigin != "https://bridge.example" || settings.directOrigin != "https://lan.bridge.example:8788" || settings.port != 8788 || len(settings.certificate.Certificate) == 0 {
		t.Fatalf("settings=%+v", settings)
	}
}

func TestLoadDirectSettingsRejectsInvalidConfiguration(t *testing.T) {
	dataDir := t.TempDir()
	for _, direct := range []string{
		`{"publicUrl":"http://bridge.example","url":"https://lan.bridge.example:8788","certFile":"a","keyFile":"b"}`,
		`{"publicUrl":"https://bridge.example","url":"https://bridge.example","certFile":"a","keyFile":"b"}`,
		`{"publicUrl":"https://bridge.example","url":"https://lan.bridge.example/path","certFile":"a","keyFile":"b"}`,
		`{"publicUrl":"https://bridge.example","url":"https://lan.bridge.example:8788","certFile":"missing.crt","keyFile":"missing.key"}`,
	} {
		config := fmt.Sprintf(`{"direct":%s}`, direct)
		if err := os.WriteFile(filepath.Join(dataDir, "desktop.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		if settings, err := loadDirectSettings(dataDir, desktopPreferences(dataDir)); err == nil || settings != nil {
			t.Fatalf("invalid config accepted: %s", direct)
		}
	}
}
