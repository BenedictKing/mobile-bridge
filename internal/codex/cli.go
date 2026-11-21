package codex

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Only explicitly selected threads use this runtime. Merely listing desktop
// history never resumes a thread or starts a second execution owner.
type cliRuntime struct {
	bridge    *Bridge
	connectMu sync.Mutex
	attachMu  sync.Mutex
	mu        sync.Mutex
	process   *acp.Process
	pending   map[string]*jsoncompat.Object
	lastUsed  map[string]time.Time // guarded by bridge.mu
	stop      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once
}

func (b *Bridge) RegisterCLI(id, cwd string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return
	}
	if b.cliThreads == nil {
		b.cliThreads = map[string]string{}
		b.cli = &cliRuntime{bridge: b, pending: map[string]*jsoncompat.Object{},
			lastUsed: map[string]time.Time{}, stop: make(chan struct{}), done: make(chan struct{})}
		go b.cli.maintain()
	}
	b.cliThreads[id] = cwd
}

func (b *Bridge) IsCLI(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.cliThreads[id]
	return ok
}

func (b *Bridge) watchCLI(id string) (*Session, error) {
	b.mu.Lock()
	c, cwd, closing := b.cli, b.cliThreads[id], b.closing
	b.mu.Unlock()
	if closing {
		return nil, errors.New("网关正在关闭")
	}
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	return b.watchCLILocked(id, c, cwd)
}

// The caller holds attachMu through the subsequent write, excluding release.
func (b *Bridge) watchCLILocked(id string, c *cliRuntime, cwd string) (*Session, error) {
	session := b.session(id)
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return session, errors.New("网关正在关闭")
	}
	ready := session.Connected && session.Owner == "cli"
	if ready {
		c.lastUsed[id] = time.Now()
	}
	b.mu.Unlock()
	if ready {
		return session, nil
	}
	if err := c.connect(); err != nil {
		return session, err
	}
	read, err := c.request("thread/read", object(map[string]any{"threadId": id, "includeTurns": false}), 30*time.Second)
	if err != nil {
		return session, err
	}
	thread, _ := read.MustGet("thread").(*jsoncompat.Object)
	if thread == nil || thread.MustGet("id") != id || !cliPathEqual(cliText(thread.MustGet("cwd")), cwd) {
		return session, errors.New("Codex CLI 会话身份或目录不匹配，未接管")
	}
	if status, ok := thread.MustGet("status").(*jsoncompat.Object); ok && status.MustGet("type") == "notLoaded" {
		history, readErr := c.request("thread/read", object(map[string]any{"threadId": id, "includeTurns": true}), 30*time.Second)
		if readErr != nil {
			return session, readErr
		}
		if saved, ok := history.MustGet("thread").(*jsoncompat.Object); ok {
			if turns := cliArray(saved.MustGet("turns")); turns != nil {
				for _, raw := range turns.Items {
					if turn, ok := raw.(*jsoncompat.Object); ok && turn.MustGet("status") == "inProgress" {
						return session, errors.New("会话历史仍显示执行中，请先在原运行时核实；未接管或重复发送")
					}
				}
			}
		}
	}
	resumed, err := c.request("thread/resume", object(map[string]any{"threadId": id, "excludeTurns": false}), 60*time.Second)
	if err != nil {
		if strings.Contains(err.Error(), "already has an active writer") {
			err = errors.New("此聊天仍由另一个 Codex 应用占用；请在原应用关闭会话，等待写入锁释放后重试")
		}
		return session, err
	}
	loaded, _ := resumed.MustGet("thread").(*jsoncompat.Object)
	if loaded == nil || loaded.MustGet("id") != id || !cliPathEqual(cliText(loaded.MustGet("cwd")), cwd) || loaded.MustGet("modelProvider") != thread.MustGet("modelProvider") {
		return session, errors.New("Codex CLI 恢复结果与源会话不匹配")
	}
	c.applyThread(loaded, resumed, true)
	b.mu.Lock()
	c.lastUsed[id] = time.Now()
	b.mu.Unlock()
	return session, nil
}

func (b *Bridge) readCLI(id string) (*Session, error) {
	b.mu.Lock()
	c, cwd := b.cli, b.cliThreads[id]
	b.mu.Unlock()
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	session := b.session(id)
	b.mu.Lock()
	ready, closing := session.Connected && session.Owner == "cli", b.closing
	b.mu.Unlock()
	if closing {
		return session, errors.New("网关正在关闭")
	}
	if ready {
		return session, nil
	}
	_, err := c.readThread(id, cwd, 30*time.Second)
	return session, err
}

// readThread observes native history without resuming or extending ownership.
// The caller holds attachMu, so publishing the read cannot overwrite a resume.
func (c *cliRuntime) readThread(id, cwd string, timeout time.Duration) (*jsoncompat.Object, error) {
	if err := c.connect(); err != nil {
		return nil, err
	}
	read, err := c.request("thread/read", object(map[string]any{"threadId": id, "includeTurns": true}), timeout)
	if err != nil {
		return nil, err
	}
	thread, _ := read.MustGet("thread").(*jsoncompat.Object)
	if thread == nil || thread.MustGet("id") != id || !cliPathEqual(cliText(thread.MustGet("cwd")), cwd) {
		return nil, errors.New("Codex CLI 会话身份或目录不匹配，未接管")
	}
	c.bridge.mu.Lock()
	session := c.bridge.sessions[id]
	connected := session != nil && session.Connected && session.Owner == "cli"
	c.bridge.mu.Unlock()
	c.applyThread(thread, nil, connected)
	return read, nil
}

func (c *cliRuntime) connect() error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	c.bridge.mu.Lock()
	closing := c.bridge.closing
	c.bridge.mu.Unlock()
	if closing {
		return errors.New("网关正在关闭")
	}
	c.mu.Lock()
	ready := c.process != nil
	c.mu.Unlock()
	if ready {
		return nil
	}
	b := c.bridge
	executable := b.Executable
	if executable == "" {
		executable = os.Getenv("GATEWAY_CODEX_BIN")
	}
	if executable == "" {
		executable, _ = exec.LookPath("codex")
	}
	if executable == "" {
		return errors.New("找不到 Codex CLI 运行时")
	}
	path := b.CLIPath
	if path == "" {
		path = filepath.Join(b.Home, "mobile-bridge-cli.sock")
	}
	stream, err := connectCLITransport(executable, b.Home, path)
	if err != nil {
		return err
	}
	process := acp.StartTransport(stream, func(p *acp.Process) {
		p.OnNotification = c.onEvent
		p.OnRequest = func(message *jsoncompat.Object, _ func(any)) { c.onRequest(message) }
		p.OnClose = func() { _ = stream.Close(); c.disconnected(p) }
		c.mu.Lock()
		c.process = p
		c.mu.Unlock()
	})
	initialized, err := c.request("initialize", object(map[string]any{
		"clientInfo":   object(map[string]any{"name": "mobile_bridge", "version": "1"}),
		"capabilities": object(map[string]any{"experimentalApi": true}),
	}), 15*time.Second)
	if err == nil {
		if !cliPathEqual(cliText(initialized.MustGet("codexHome")), b.Home) {
			err = errors.New("Codex CLI 数据目录与网关不匹配: " + cliText(initialized.MustGet("codexHome")))
		}
	}
	if err == nil {
		err = process.Notify("initialized", jsoncompat.NewObject())
	}
	if err != nil {
		process.Close()
	}
	return err
}

func cliPathEqual(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	canonicalLeft, leftErr := filepath.EvalSymlinks(left)
	canonicalRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && canonicalLeft == canonicalRight
}

func (c *cliRuntime) request(method string, params *jsoncompat.Object, timeout time.Duration) (*jsoncompat.Object, error) {
	c.mu.Lock()
	process := c.process
	c.mu.Unlock()
	if process == nil {
		return nil, errors.New("Codex CLI 连接已断开")
	}
	value, err := process.Request(method, params, timeout)
	if err != nil {
		return nil, err
	}
	result, ok := value.(*jsoncompat.Object)
	if !ok {
		return nil, errors.New("Codex CLI 返回了无效响应")
	}
	return result, nil
}

func (c *cliRuntime) disconnected(process *acp.Process) {
	c.mu.Lock()
	if c.process != process {
		c.mu.Unlock()
		return
	}
	c.process = nil
	c.pending = map[string]*jsoncompat.Object{}
	c.mu.Unlock()
	b := c.bridge
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.cliThreads {
		if session := b.sessions[id]; session != nil {
			session.Connected = false
			session.Error = "Codex CLI 连接已断开，正在重新接入原运行时"
			c.changedLocked(session)
		}
	}
}

func (c *cliRuntime) changedLocked(session *Session) {
	session.Sequence++
	close(session.Changed)
	session.Changed = make(chan struct{})
}

func (b *Bridge) closeCLI() {
	b.mu.Lock()
	c := b.cli
	b.mu.Unlock()
	if c == nil {
		return
	}
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	c.mu.Lock()
	p := c.process
	c.mu.Unlock()
	if p != nil {
		p.Close()
	} // Disconnect only; the official runtime keeps running.
}
