// Package acpagents ports bridge/agents' local agent discovery to the Go
// gateway: the same env overrides, PATH probes, home-directory markers and
// npx fallbacks that decided which ACP agents the Python gateway exposed.
package acpagents

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Profile is one discoverable ACP agent: how to launch it and the adapter
// constants the HTTP surface needs (label, capabilities, effort folding,
// handshake budget, placeholder canonicalization).
type Profile struct {
	Kind              string
	Label             string
	Command           []string // nil when the agent is not detectable locally
	InitializeTimeout time.Duration
	Capabilities      []string
	EffortMap         map[string]string
	Canonical         func(id string) string

	// SteerMethod names the steering RPC (claude/dsh/mcode); ModeMap translates the
	// bridge's plan/default work modes onto the agent's own mode ids.
	// ModeOptionID names the dialect whose collaboration surface is the config
	// option with that id (dsh: collaboration_mode — its modes object is a
	// permission-preset surface).
	SteerMethod  string
	ModeMap      map[string]string
	ModeOptionID string

	// LazySessions marks agents whose session/new only reserves an id: the
	// agent materializes the session in its own store on the first prompt
	// (zcode maps the placeholder to a backend sess_*, workbuddy writes the
	// transcript's cwd). Until that first turn the session exists only in
	// this process — session/load fails after a restart — so the gateway
	// warms such a session up when it is created on someone's behalf.
	LazySessions bool
}

// Discover probes the machine like run.py's --agents auto. env abstracts
// os.Getenv for tests; home is the user's home directory.
func Discover(env func(string) string, home string) []Profile {
	return []Profile{
		kimiProfile(env, home),
		mcodeProfile(env, home),
		claudeProfile(env, home),
		dshProfile(env, home),
		zcodeProfile(env, home),
		opencodeProfile(env, home),
		workbuddyProfile(env, home),
		acpAgentProfile(env, "acpagent", "ACP Agent", "ACP_AGENT_BIN"),
		acpAgentProfile(env, "acpagent-review", "ACP Agent Review", "ACP_AGENT_REVIEW_BIN"),
	}
}

// KnownKind reports whether kind is a recognized agent kind (codex included).
func KnownKind(kind string) bool {
	switch kind {
	case "codex", "kimi", "mcode", "claude", "dsh", "zcode", "opencode", "workbuddy", "acpagent", "acpagent-review":
		return true
	}
	return false
}

// ParseKinds mirrors run.py's --agents parsing: "auto" enables every
// discoverable agent (nil, nil); otherwise a comma list validated against
// the known kinds.
func ParseKinds(value string) ([]string, error) {
	kinds := []string{}
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			kinds = append(kinds, trimmed)
		}
	}
	if len(kinds) == 1 && kinds[0] == "auto" {
		return nil, nil
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	for _, kind := range kinds {
		if !KnownKind(kind) {
			return nil, errAgents()
		}
	}
	return kinds, nil
}

type agentsError struct{}

func (agentsError) Error() string {
	return "agents 支持 codex、kimi、mcode、claude、dsh、zcode、opencode、workbuddy、acpagent、acpagent-review，或 auto（自动探测本机已安装的）"
}

func errAgents() error { return agentsError{} }

// SplitArgv is a small shlex.split for env-provided launch commands, honoring
// single and double quotes.
func SplitArgv(value string) []string {
	argv := []string{}
	var current strings.Builder
	quote := byte(0)
	flush := func() {
		if current.Len() > 0 {
			argv = append(argv, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else if ch == '\\' && quote == '"' && i+1 < len(value) {
				i++
				current.WriteByte(value[i])
			} else {
				current.WriteByte(ch)
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '\\' && i+1 < len(value):
			i++
			current.WriteByte(value[i])
		case ch == ' ' || ch == '\t':
			flush()
		default:
			current.WriteByte(ch)
		}
	}
	flush()
	return argv
}

func lookupPath(name string) string {
	if path, err := exec.LookPath(name); err == nil && executableFile(path) {
		return path
	}
	return ""
}

func localBinary(home, defaultRel, name string) string {
	candidate := filepath.Join(home, filepath.FromSlash(defaultRel), name)
	if executableFile(candidate) {
		return candidate
	}
	return ""
}

// executableFile rejects incomplete installs without running the CLI. Go's
// exec does not provide a shell's fallback for text files without a shebang,
// including the placeholder left when a package's postinstall did not run.
// Header checks are only a discovery filter, not a full loader validation.
func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var header [4]byte
	n, _ := io.ReadFull(file, header[:])
	if runtime.GOOS != "windows" && n >= 2 && string(header[:2]) == "#!" {
		return true
	}
	switch runtime.GOOS {
	case "windows":
		return n >= 2 && string(header[:2]) == "MZ"
	case "linux":
		return n == 4 && string(header[:]) == "\x7fELF"
	case "darwin":
		switch string(header[:n]) {
		case "\xfe\xed\xfa\xce", "\xce\xfa\xed\xfe", "\xfe\xed\xfa\xcf", "\xcf\xfa\xed\xfe",
			"\xca\xfe\xba\xbe", "\xbe\xba\xfe\xca", "\xca\xfe\xba\xbf", "\xbf\xba\xfe\xca":
			return true
		}
		return false
	default:
		return n > 0
	}
}

// discoverExecutable mirrors bridge/agents/acp.py: env override, then
// ~/<default_rel>/<name>, then PATH.
func discoverExecutable(env func(string) string, home, envVar, defaultRel, name string) string {
	if candidate := env(envVar); candidate != "" {
		return candidate
	}
	if local := localBinary(home, defaultRel, name); local != "" {
		return local
	}
	return lookupPath(name)
}

func kimiProfile(env func(string) string, home string) Profile {
	return Profile{
		Kind: "kimi", Label: "Kimi",
		Command:           subcommandArgv(discoverExecutable(env, home, "KIMI_BIN", ".kimi-code/bin", "kimi")),
		InitializeTimeout: 10 * time.Second,
		Capabilities:      []string{"skills", "fork"},
		EffortMap:         map[string]string{"none": "low", "minimal": "low", "low": "low", "medium": "high", "high": "high", "xhigh": "max", "max": "max", "ultra": "max"},
	}
}

func mcodeProfile(env func(string) string, home string) Profile {
	return Profile{
		Kind: "mcode", Label: "MiniMax",
		Command:           subcommandArgv(discoverExecutable(env, home, "MCODE_BIN", ".minimax-code/bin", "mcode")),
		InitializeTimeout: 10 * time.Second,
		Capabilities:      []string{"goals", "steer", "fork"},
		EffortMap:         map[string]string{"none": "default", "minimal": "low", "low": "low", "medium": "medium", "high": "high", "xhigh": "max", "max": "max", "ultra": "max"},
		SteerMethod:       "mcode/session/steer",
	}
}

func claudeProfile(env func(string) string, home string) Profile {
	argv := wrapperArgv(env, "CLAUDE_ACP_BIN", "claude-agent-acp", "@agentclientprotocol/claude-agent-acp")
	// The npx fallback does not count: any machine with npx would otherwise
	// auto-enable claude and trigger the ~130s first-run download.
	available := env("CLAUDE_ACP_BIN") != "" ||
		lookupPath("claude-agent-acp") != "" || lookupPath("claude") != ""
	if !available {
		argv = nil
	}
	return Profile{
		Kind: "claude", Label: "Claude",
		Command:           argv,
		InitializeTimeout: 180 * time.Second,
		Capabilities:      []string{"steer", "fork"},
		EffortMap:         map[string]string{"none": "default", "minimal": "low", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max", "ultra": "max"},
		SteerMethod:       "_session/steering",
	}
}

func dshProfile(env func(string) string, home string) Profile {
	argv := wrapperArgv(env, "DSH_ACP_BIN", "dsh-acp", "@openma/deepseek-harness-acp")
	// A ~/.dsh directory counts: dsh may only ever have run via npx yet
	// still leave its credentials and config there.
	available := env("DSH_ACP_BIN") != "" || lookupPath("dsh-acp") != "" ||
		lookupPath("dsh") != "" || dirExists(DshHome(env, home))
	if !available {
		argv = nil
	}
	return Profile{
		Kind: "dsh", Label: "DeepSeek",
		Command:           argv,
		InitializeTimeout: 180 * time.Second,
		Capabilities:      []string{"steer", "fork"},
		EffortMap:         map[string]string{"none": "off", "minimal": "low", "low": "low", "medium": "high", "high": "high", "xhigh": "max", "max": "max", "ultra": "max"},
		SteerMethod:       "_session/steering",
		ModeOptionID:      "collaboration_mode",
	}
}

func zcodeProfile(env func(string) string, home string) Profile {
	argv := wrapperArgv(env, "ZCODE_ACP_BIN", "zcode-acp-server", "zcode-acp-server")
	if argv == nil {
		argv = wrapperArgv(env, "", "zcode-acp", "zcode-acp-server")
	}
	// A ~/.zcode directory counts: the wrapper reads its credentials from
	// there, so a machine that only ever ran the desktop app still hosts
	// usable sessions.
	available := env("ZCODE_ACP_BIN") != "" || lookupPath("zcode-acp-server") != "" ||
		lookupPath("zcode-acp") != "" || lookupPath("zcode") != "" ||
		dirExists(zcodeHome(env, home))
	if !available {
		argv = nil
	}
	return Profile{
		Kind: "zcode", Label: "ZCode",
		Command:           argv,
		InitializeTimeout: 180 * time.Second,
		Capabilities:      []string{"fork"},
		EffortMap:         map[string]string{"none": "off", "minimal": "enabled", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max", "ultra": "ultra"},
		Canonical:         zcodeCanonicalFactory(env, home),
		ModeMap:           map[string]string{"plan": "plan", "default": "yolo"},
		// The wrapper's placeholder → sess_* mapping (and the backend row in
		// ~/.zcode/cli/db) is written by the first prompt, not by session/new.
		LazySessions: true,
	}
}

// opencodeProfile launches OpenCode's built-in ACP server (`opencode acp`).
// Only a real install enables auto-discovery: the CLI itself is the server,
// so no npx fallback and no data-directory marker — a leftover
// ~/.local/share/opencode from a removed install must not auto-enable it.
func opencodeProfile(env func(string) string, home string) Profile {
	return Profile{
		Kind: "opencode", Label: "OpenCode",
		Command:           subcommandArgv(discoverExecutable(env, home, "OPENCODE_BIN", ".opencode/bin", "opencode")),
		InitializeTimeout: 30 * time.Second,
		Capabilities:      []string{"skills", "fork"},
		// OpenCode advertises per-model effort variants ("default" always,
		// low/medium/high commonly, minimal/xhigh on some models); the bridge
		// ladder folds onto the shared set and the catalog only lists the
		// variants the current model actually offers.
		EffortMap: map[string]string{"none": "low", "minimal": "minimal", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "high", "ultra": "high"},
		ModeMap:   map[string]string{"plan": "plan", "default": "build"},
	}
}

// subcommandArgv builds the [executable, "acp"] launch used by the CLIs that
// expose ACP behind a subcommand (kimi/mcode/opencode).
func subcommandArgv(executable string) []string {
	if executable == "" {
		return nil
	}
	return []string{executable, "acp"}
}

// workbuddyProfile launches CodeBuddy Code's built-in ACP mode
// (`codebuddy --acp`). The CLI itself is the ACP server — no wrapper and no
// npx fallback; a leftover ~/.codebuddy alone must not auto-enable it, so
// only a real binary on PATH (or CODEBUDDY_BIN) counts. Cold session/new
// takes ~20s (the CLI loads its skill/command catalog before answering),
// hence the generous handshake budget.
func workbuddyProfile(env func(string) string, home string) Profile {
	executable := discoverExecutable(env, home, "CODEBUDDY_BIN", ".codebuddy/bin", "codebuddy")
	var argv []string
	if executable != "" {
		argv = []string{executable, "--acp"}
	}
	return Profile{
		// 用户可见名跟随 CLI 品牌（CodeBuddy）；kind 仍为 workbuddy（池与
		// 存储标识，历史兼容）。桌面 App 才叫 WorkBuddy。
		Kind: "workbuddy", Label: "CodeBuddy",
		Command:           argv,
		InitializeTimeout: 60 * time.Second,
		Capabilities:      []string{},
		// codebuddy writes its transcript file (with the cwd session/list
		// filters on) on the first message, not on session/new.
		LazySessions: true,
	}
}

// acpAgentProfile registers the self-hosted acp-agent runtime. It is opt-in
// only: the launch needs a model endpoint, an absolute --state-dir and a
// team role (member vs reviewer), so the env var carries the full argv,
// e.g. `acp-agent acp --role member --state-dir /abs --base-url … --model …`.
// A bare `acp-agent` on PATH never auto-enables it. The member and reviewer
// kinds are separate because --role is fixed per process; give each its own
// --state-dir, since session/list returns every session under that dir and
// a shared dir would let one kind claim the other's sessions. Sessions
// persist on session/new, so no lazy warm-up is needed.
func acpAgentProfile(env func(string) string, kind, label, envVar string) Profile {
	var argv []string
	if command := env(envVar); command != "" {
		argv = SplitArgv(command)
	}
	return Profile{
		Kind: kind, Label: label,
		Command:           argv,
		InitializeTimeout: 30 * time.Second,
		Capabilities:      []string{"steer"},
		// acp-agent thinking levels: off|minimal|low|medium|high|xhigh.
		EffortMap:   map[string]string{"none": "off", "minimal": "minimal", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "xhigh", "ultra": "xhigh"},
		SteerMethod: "_session/steering",
	}
}

// wrapperArgv mirrors the zcode/claude/dsh discovery: env override, then a
// PATH binary, then npx -y (only when npx itself exists).
func wrapperArgv(env func(string) string, envVar, binary, npmPackage string) []string {
	if candidate := env(envVar); candidate != "" {
		return SplitArgv(candidate)
	}
	if path := lookupPath(binary); path != "" {
		return []string{path}
	}
	if lookupPath("npx") != "" {
		return []string{"npx", "-y", npmPackage}
	}
	return nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// DshHome is the DeepSeek Harness data directory (credentials, session
// transcripts and the app's projection cache).
func DshHome(env func(string) string, home string) string {
	if explicit := env("DSH_HOME"); explicit != "" {
		return explicit
	}
	return filepath.Join(home, ".dsh")
}

func zcodeHome(env func(string) string, home string) string {
	if explicit := env("ZCODE_HOME"); explicit != "" {
		return explicit
	}
	return filepath.Join(home, ".zcode")
}

// ZcodeDBPath is the SQLite database the ZCode desktop app and the zcode-acp
// wrapper share for sessions, messages and parts. The gateway reads it
// read-only to follow turns the desktop app drives.
func ZcodeDBPath(env func(string) string, home string) string {
	for _, key := range []string{"ZCODE_SESSION_DB_PATH", "ZCODE_SESSION_DB"} {
		if explicit := env(key); explicit != "" {
			if explicit == "~" {
				return home
			}
			if strings.HasPrefix(explicit, "~/") {
				return filepath.Join(home, explicit[2:])
			}
			return explicit
		}
	}
	return filepath.Join(zcodeHome(env, home), "cli", "db", "db.sqlite")
}

// zcodeCanonicalFactory resolves zcode's lazy session placeholders: the
// wrapper records placeholder → backend sess_* in
// <zcode-home>/v2/acp-lazy-sessions.json once the first prompt materializes
// the backend session. Re-read per call like Python's _read_json.
func zcodeCanonicalFactory(env func(string) string, home string) func(string) string {
	return func(id string) string {
		path := filepath.Join(zcodeHome(env, home), "v2", "acp-lazy-sessions.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			return ""
		}
		object, _ := value.(*jsoncompat.Object)
		if object == nil {
			return ""
		}
		entry, _ := object.MustGet(id).(*jsoncompat.Object)
		if entry == nil {
			return ""
		}
		sid, _ := entry.MustGet("zcodeSid").(string)
		return sid
	}
}
