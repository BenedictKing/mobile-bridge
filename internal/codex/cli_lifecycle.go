package codex

import (
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

const cliIdleDelay = 30 * time.Second
const cliLifecycleTimeout = 5 * time.Second

func cliStatus(value any) string {
	status, _ := value.(*jsoncompat.Object)
	if status == nil {
		return ""
	}
	return cliText(status.MustGet("type"))
}

func (c *cliRuntime) touch(id string) {
	c.bridge.mu.Lock()
	if _, tracked := c.lastUsed[id]; tracked {
		c.lastUsed[id] = time.Now()
	}
	c.bridge.mu.Unlock()
}

// detachedLocked preserves history and the durable CLI route. A subsequent
// explicit write resumes the same thread; passive reads never resubscribe.
func (c *cliRuntime) detachedLocked(session *Session) {
	delete(c.lastUsed, session.ID)
	session.Owner, session.Connected, session.Error = "", false, ""
	if state, ok := session.State.(*jsoncompat.Object); ok {
		state.Set("writerReleased", true)
	}
}

func (c *cliRuntime) maintain() {
	defer close(c.done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-ticker.C:
			c.releaseIdle(now)
		}
	}
}

func (c *cliRuntime) lifecycleRequest(method string, params *jsoncompat.Object) (*jsoncompat.Object, error) {
	select {
	case <-c.stop:
		return nil, errors.New("网关正在关闭")
	default:
		return c.request(method, params, cliLifecycleTimeout)
	}
}

// eligibleLocked is only a local preflight. Native status and background work
// are checked again before unsubscribing, without interrupting any operation.
func (c *cliRuntime) eligibleLocked(id string, now time.Time) bool {
	used, tracked := c.lastUsed[id]
	session := c.bridge.sessions[id]
	if !tracked || now.Sub(used) < cliIdleDelay || session == nil || c.bridge.closing {
		return false
	}
	state, _ := session.State.(*jsoncompat.Object)
	if state == nil || cliStatus(state.MustGet("threadRuntimeStatus")) != "idle" || len(model.PendingRequests(state).Items) != 0 {
		return false
	}
	for _, raw := range model.OrderedTurns(state).Items {
		turn, _ := raw.(*jsoncompat.Object)
		if turn != nil && turn.MustGet("status") == "inProgress" {
			return false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.pending {
		if strings.HasPrefix(key, id+"\x00") {
			return false
		}
	}
	return true
}

func (c *cliRuntime) releaseIdle(now time.Time) {
	// Writes already in progress take priority; a maintenance tick never queues
	// behind them and cannot slip between a resume and its subsequent send.
	if !c.attachMu.TryLock() {
		return
	}
	defer c.attachMu.Unlock()
	c.bridge.mu.Lock()
	ids := make([]string, 0, len(c.lastUsed))
	for id := range c.lastUsed {
		if c.eligibleLocked(id, now) {
			ids = append(ids, id)
		}
	}
	c.bridge.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		select {
		case <-c.stop:
			return
		default:
		}
		if err := c.releaseThread(id, now); err != nil {
			log.Printf("Codex 空闲会话释放未完成: thread=%s: %v", id, err)
		}
		c.touch(id) // Back off after a busy/unsupported/failed native probe.
	}
}

func (c *cliRuntime) releaseThread(id string, now time.Time) error {
	c.bridge.mu.Lock()
	if !c.eligibleLocked(id, now) {
		c.bridge.mu.Unlock()
		return nil
	}
	used, sequence := c.lastUsed[id], c.bridge.sessions[id].Sequence
	cwd := c.bridge.cliThreads[id]
	c.bridge.mu.Unlock()
	read, err := c.lifecycleRequest("thread/read", object(map[string]any{"threadId": id, "includeTurns": false}))
	if err != nil {
		return err
	}
	thread, _ := read.MustGet("thread").(*jsoncompat.Object)
	if thread == nil || thread.MustGet("id") != id || !cliPathEqual(cliText(thread.MustGet("cwd")), cwd) {
		return errors.New("Codex 空闲检查返回了不匹配的会话")
	}
	status := cliStatus(thread.MustGet("status"))
	if status != "idle" && status != "notLoaded" {
		return nil
	}
	if status == "idle" {
		quiet, err := c.noBackgroundTerminals(id)
		if err != nil || !quiet {
			return err
		}
		quiet, err = c.noBusyDescendants(id)
		if err != nil || !quiet {
			return err
		}
	}
	c.bridge.mu.Lock()
	unchanged := c.eligibleLocked(id, now) && c.lastUsed[id].Equal(used) && c.bridge.sessions[id].Sequence == sequence
	c.bridge.mu.Unlock()
	if !unchanged {
		return nil
	}
	if status != "notLoaded" {
		response, err := c.lifecycleRequest("thread/unsubscribe", object(map[string]any{"threadId": id}))
		if err == nil {
			switch response.MustGet("status") {
			case "unsubscribed", "notSubscribed", "notLoaded":
			default:
				err = errors.New("Codex 返回了无效的取消订阅状态")
			}
		}
		if err != nil {
			// A timeout may have unsubscribed successfully. Require a fresh resume
			// before writing, while retaining the retry for a possibly live owner.
			c.bridge.mu.Lock()
			session := c.bridge.sessions[id]
			session.Owner, session.Connected = "", false
			session.Error = "会话释放结果尚未确认，继续发送时将重新接入原会话"
			c.changedLocked(session)
			c.bridge.mu.Unlock()
			return err
		}
	}
	c.bridge.mu.Lock()
	session := c.bridge.sessions[id]
	c.detachedLocked(session)
	if status == "notLoaded" {
		state, _ := session.State.(*jsoncompat.Object)
		state.Set("threadRuntimeStatus", object(map[string]any{"type": "notLoaded"}))
	}
	c.changedLocked(session)
	c.bridge.mu.Unlock()
	return nil
}

func (c *cliRuntime) noBackgroundTerminals(id string) (bool, error) {
	result, err := c.lifecycleRequest("thread/backgroundTerminals/list", object(map[string]any{"threadId": id, "limit": 1}))
	if err != nil {
		return false, err
	}
	data := cliArray(result.MustGet("data"))
	if data == nil {
		return false, errors.New("Codex 未返回后台终端列表，保留会话占用")
	}
	return len(data.Items) == 0 && cliText(result.MustGet("nextCursor")) == "", nil
}

func (c *cliRuntime) noBusyDescendants(id string) (bool, error) {
	cursor := ""
	for page := 0; page < 10; page++ {
		params := object(map[string]any{
			"ancestorThreadId": id, "sourceKinds": &jsoncompat.Array{Items: []any{
				"subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther",
			}},
			"modelProviders": &jsoncompat.Array{}, "limit": 100,
		})
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		result, err := c.lifecycleRequest("thread/list", params)
		if err != nil {
			return false, err
		}
		data := cliArray(result.MustGet("data"))
		if data == nil {
			return false, errors.New("Codex 未返回子 Agent 列表，保留会话占用")
		}
		for _, raw := range data.Items {
			child, _ := raw.(*jsoncompat.Object)
			if child == nil || cliText(child.MustGet("id")) == "" {
				return false, errors.New("Codex 返回了无效的子 Agent 状态")
			}
			switch cliStatus(child.MustGet("status")) {
			case "notLoaded":
			case "idle":
				quiet, err := c.noBackgroundTerminals(cliText(child.MustGet("id")))
				if err != nil || !quiet {
					return false, err
				}
			default:
				return false, nil
			}
		}
		next := cliText(result.MustGet("nextCursor"))
		if next == "" {
			return true, nil
		}
		if next == cursor {
			return false, errors.New("Codex 子 Agent 分页游标未前进")
		}
		cursor = next
	}
	return false, errors.New("Codex 子 Agent 列表超过检查上限，保留会话占用")
}
