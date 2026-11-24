package codex

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func CLISupported() bool { return runtime.GOOS != "windows" }

// ClaimCLI acquires the original thread without exposing a new route until its
// ownership has been persisted. No turn is submitted during a handoff.
func (b *Bridge) ClaimCLI(id, cwd string, persist func() error) error {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return errors.New("网关正在关闭")
	}
	c := b.ensureCLIRuntimeLocked()
	b.mu.Unlock()
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	loaded, resumed, err := c.resumeThread(id, cwd, true)
	if err != nil {
		return err
	}
	if err := persist(); err != nil {
		if releaseErr := c.unsubscribe(id); releaseErr != nil {
			return fmt.Errorf("执行归属保存失败，写锁释放结果待确认: %v; %w", releaseErr, err)
		}
		return err
	}
	session := b.session(id)
	b.mu.Lock()
	b.cliThreads[id] = cwd
	sequence := session.Sequence
	b.mu.Unlock()
	if !c.applyThread(loaded, resumed, true, sequence) {
		_, err = c.readThreadSnapshot(id, cwd, 30*time.Second, resumed, true)
	}
	b.mu.Lock()
	c.lastUsed[id] = time.Now()
	b.mu.Unlock()
	return err
}

// HandOffCLI releases only this gateway's idle native subscription. A persistence
// failure retains the CLI route so a subsequent explicit write can resume it.
func (b *Bridge) HandOffCLI(id, cwd string, persist func() error) error {
	b.mu.Lock()
	c, closing := b.cli, b.closing
	b.mu.Unlock()
	if closing {
		return errors.New("网关正在关闭")
	}
	if c != nil {
		c.attachMu.Lock()
		defer c.attachMu.Unlock()
		if err := c.connect(); err != nil {
			return err
		}
		read, err := c.lifecycleRequest("thread/read", object(map[string]any{"threadId": id, "includeTurns": false}))
		if err != nil {
			return err
		}
		thread, _ := read.MustGet("thread").(*jsoncompat.Object)
		if thread == nil || thread.MustGet("id") != id || !cliPathEqual(cliText(thread.MustGet("cwd")), cwd) {
			return errors.New("Codex 交接返回了不匹配的会话")
		}
		status := cliStatus(thread.MustGet("status"))
		if status != "idle" && status != "notLoaded" {
			return errors.New("会话仍在执行或等待审批，暂不能交接")
		}
		if status == "idle" {
			if err := c.requireQuiet(id); err != nil {
				return err
			}
			if err := c.unsubscribe(id); err != nil {
				b.mu.Lock()
				if session := b.sessions[id]; session != nil {
					session.Owner, session.Connected = "", false
					session.Error = "会话释放结果尚未确认，继续发送时将重新接入原会话"
					c.changedLocked(session)
				}
				b.mu.Unlock()
				return err
			}
		}
		b.mu.Lock()
		if session := b.sessions[id]; session != nil {
			c.detachedLocked(session)
			c.changedLocked(session)
		}
		b.mu.Unlock()
	}
	if err := persist(); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.cliThreads, id)
	if session := b.sessions[id]; session != nil {
		session.Owner, session.Error, session.Connected, session.Revision = "", "", false, nil
		if state, ok := session.State.(*jsoncompat.Object); ok {
			state.Set("writerReleased", false)
			state.Set("threadRuntimeStatus", object(map[string]any{"type": "notLoaded"}))
		}
		if c != nil {
			c.changedLocked(session)
		}
	}
	b.mu.Unlock()
	return nil
}

func (c *cliRuntime) requireQuiet(id string) error {
	for _, quiet := range []func(string) (bool, error){c.noBackgroundTerminals, c.noBusyDescendants} {
		ok, err := quiet(id)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("后台终端或子代理仍在运行，暂不能交接")
		}
	}
	return nil
}

func (c *cliRuntime) unsubscribe(id string) error {
	response, err := c.lifecycleRequest("thread/unsubscribe", object(map[string]any{"threadId": id}))
	if err != nil {
		return err
	}
	switch response.MustGet("status") {
	case "unsubscribed", "notSubscribed", "notLoaded":
		return nil
	default:
		return errors.New("Codex 返回了无效的取消订阅状态")
	}
}
