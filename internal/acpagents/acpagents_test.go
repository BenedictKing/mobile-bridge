package acpagents

import (
	"os"
	"path/filepath"
	"testing"
)

func emptyEnv(string) string { return "" }

func TestSplitArgv(t *testing.T) {
	cases := []struct{ input string }{
		{`zcode-acp-server`},
		{`npx -y zcode-acp-server`},
		{`/opt/bin/my agent --flag "quoted value" 'single'`},
	}
	first := SplitArgv(cases[0].input)
	if len(first) != 1 || first[0] != "zcode-acp-server" {
		t.Fatalf("single=%v", first)
	}
	second := SplitArgv(cases[1].input)
	if len(second) != 3 || second[1] != "-y" {
		t.Fatalf("npx=%v", second)
	}
	third := SplitArgv(cases[2].input)
	if len(third) != 5 || third[3] != "quoted value" || third[4] != "single" {
		t.Fatalf("quoted=%v", third)
	}
}

func TestParseKinds(t *testing.T) {
	if kinds, err := ParseKinds("auto"); err != nil || kinds != nil {
		t.Fatalf("auto=%v err=%v", kinds, err)
	}
	if kinds, err := ParseKinds(" codex, zcode "); err != nil || len(kinds) != 2 || kinds[0] != "codex" {
		t.Fatalf("list=%v err=%v", kinds, err)
	}
	if _, err := ParseKinds("codex,nope"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestDiscoverZcodeViaHomeMarker(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".zcode", "v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	var zcode *Profile
	for _, profile := range Discover(emptyEnv, home) {
		if profile.Kind == "zcode" {
			zcode = &profile
		}
	}
	if zcode == nil || zcode.Command == nil {
		t.Fatalf("zcode not discovered via ~/.zcode marker: %+v", zcode)
	}
	// The npx fallback requires npx on PATH; on a dev machine it exists, on
	// CI it may not — only the argv shape matters here.
	if len(zcode.Command) < 1 {
		t.Fatalf("empty command: %v", zcode.Command)
	}
	if zcode.Label != "ZCode" || zcode.InitializeTimeout <= 0 {
		t.Fatalf("profile constants: %+v", zcode)
	}
}

func TestZcodeCanonicalResolvesLazyMap(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".zcode", "v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	lazy := filepath.Join(home, ".zcode", "v2", "acp-lazy-sessions.json")
	if err := os.WriteFile(lazy, []byte(`{
  "76e93fcf": {"cwd": "/tmp", "createdAt": 1791631943345},
  "d710a0b9": {"cwd": "/w", "zcodeSid": "sess_d96ced3f", "createdAt": 1791638864065}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolve := zcodeCanonicalFactory(emptyEnv, home)
	if got := resolve("d710a0b9"); got != "sess_d96ced3f" {
		t.Fatalf("canonical=%q", got)
	}
	if got := resolve("76e93fcf"); got != "" {
		t.Fatalf("unmaterialized placeholder resolved to %q", got)
	}
	if got := resolve("missing"); got != "" {
		t.Fatalf("missing id resolved to %q", got)
	}
}

func TestZcodeCanonicalHonorsZcodeHomeEnv(t *testing.T) {
	home := t.TempDir()
	alt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(alt, "v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	lazy := filepath.Join(alt, "v2", "acp-lazy-sessions.json")
	if err := os.WriteFile(lazy, []byte(`{"a": {"zcodeSid": "sess-x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(key string) string {
		if key == "ZCODE_HOME" {
			return alt
		}
		return ""
	}
	if got := zcodeCanonicalFactory(env, home)("a"); got != "sess-x" {
		t.Fatalf("canonical=%q", got)
	}
}

func TestDiscoverEnvOverrides(t *testing.T) {
	home := t.TempDir()
	env := func(key string) string {
		switch key {
		case "KIMI_BIN":
			return "/opt/kimi/bin/kimi"
		case "ZCODE_ACP_BIN":
			return `/opt/tools/zcode-acp-server --port 0`
		}
		return ""
	}
	profiles := Discover(env, home)
	var kimi, zcode *Profile
	for _, profile := range profiles {
		switch profile.Kind {
		case "kimi":
			copy := profile
			kimi = &copy
		case "zcode":
			copy := profile
			zcode = &copy
		}
	}
	if kimi == nil || len(kimi.Command) != 2 || kimi.Command[0] != "/opt/kimi/bin/kimi" || kimi.Command[1] != "acp" {
		t.Fatalf("kimi argv=%v", kimi.Command)
	}
	if zcode == nil || len(zcode.Command) != 3 || zcode.Command[0] != "/opt/tools/zcode-acp-server" {
		t.Fatalf("zcode argv=%v", zcode.Command)
	}
}

func TestDiscoverKimiViaHomeDirectoryLayout(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".kimi-code", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	kimi := filepath.Join(bin, "kimi")
	if err := os.WriteFile(kimi, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profiles := Discover(emptyEnv, home)
	var found *Profile
	for _, profile := range profiles {
		if profile.Kind == "kimi" {
			copy := profile
			found = &copy
		}
	}
	if found == nil || found.Command == nil || found.Command[0] != kimi {
		t.Fatalf("kimi command=%v", found)
	}
}
