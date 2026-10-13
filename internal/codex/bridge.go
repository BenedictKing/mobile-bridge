// Package codex owns the local desktop IPC side of the Go gateway. It keeps
// the latest authoritative snapshot for watched conversations and applies
// stream patches with revision checks before exposing it to HTTP/service.
package codex

import (
	"errors"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/ipc"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/transport"
)

type Session struct {
	ID        string
	Owner     string
	State     any
	Revision  any
	Connected bool
	Sequence  int
	Error     string
	Changed   chan struct{}
}

type Bridge struct {
	Host     string
	IPC      *ipc.Desktop
	mu       sync.Mutex
	sessions map[string]*Session
}

func New(home, endpoint string) *Bridge {
	if endpoint == "" {
		endpoint = transport.CurrentOSEndpoint(home)
	}
	bridge := &Bridge{Host: "local", IPC: ipc.NewDesktop(endpoint), sessions: map[string]*Session{}}
	bridge.IPC.OnEvent = bridge.handleEvent
	bridge.IPC.OnDisconnect = bridge.handleDisconnect
	return bridge
}

func (b *Bridge) session(id string) *Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session := b.sessions[id]; session != nil {
		return session
	}
	session := &Session{ID: id, Changed: make(chan struct{})}
	b.sessions[id] = session
	return session
}

// Watch attaches to a desktop conversation. Following is deliberately kept
// active after the short wait: the owner may publish its first snapshot later.
func (b *Bridge) Watch(id string, timeout time.Duration) (*Session, error) {
	if err := b.IPC.Connect(timeout); err != nil {
		return nil, err
	}
	session := b.session(id)
	b.mu.Lock()
	if session.Connected {
		b.mu.Unlock()
		return session, nil
	}
	session.Error = ""
	b.mu.Unlock()
	response, err := b.IPC.Request("thread-owner-discovery", object(map[string]any{
		"hostId": b.Host, "conversationId": id,
	}), "", b.Host, 6*time.Second)
	if err == nil {
		if owner, ok := response.MustGet("handledByClientId").(string); ok {
			b.mu.Lock()
			session.Owner = owner
			b.mu.Unlock()
		}
	}
	if err := b.IPC.Follow(id, session.Owner, b.Host, true); err != nil {
		return session, err
	}
	select {
	case <-session.Changed:
		return session, nil
	case <-time.After(timeout):
		return session, nil
	}
}

func (b *Bridge) View(id string) (*jsoncompat.Object, bool) {
	b.mu.Lock()
	session := b.sessions[id]
	if session == nil {
		b.mu.Unlock()
		return nil, false
	}
	state, _ := session.State.(*jsoncompat.Object)
	if state == nil {
		b.mu.Unlock()
		return nil, false
	}
	copy := jsoncompat.Clone(state)
	result, _ := copy.(*jsoncompat.Object)
	b.mu.Unlock()
	return result, true
}

// Snapshot returns a copied state and the monotonic stream sequence.
func (b *Bridge) Snapshot(id string) (*jsoncompat.Object, int, bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions[id]
	if session == nil {
		return nil, 0, false, ""
	}
	state, _ := session.State.(*jsoncompat.Object)
	if state == nil {
		return nil, session.Sequence, session.Connected, session.Error
	}
	copy, _ := jsoncompat.Clone(state).(*jsoncompat.Object)
	return copy, session.Sequence, session.Connected, session.Error
}

func (b *Bridge) Wait(id string, timeout time.Duration) *Session {
	b.mu.Lock()
	session := b.sessions[id]
	if session == nil {
		b.mu.Unlock()
		return nil
	}
	changed := session.Changed
	b.mu.Unlock()
	select {
	case <-changed:
	case <-time.After(timeout):
	}
	return session
}

func (b *Bridge) handleEvent(message *jsoncompat.Object) {
	if message.MustGet("method") != "thread-stream-state-changed" {
		return
	}
	if message.MustGet("version") != jsoncompat.Num(11) {
		return
	}
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil || params.MustGet("hostId") != b.Host {
		return
	}
	id, _ := params.MustGet("conversationId").(string)
	change, _ := params.MustGet("change").(*jsoncompat.Object)
	if id == "" || change == nil {
		return
	}
	session := b.session(id)
	b.mu.Lock()
	defer b.mu.Unlock()
	if owner, ok := message.MustGet("sourceClientId").(string); ok && session.Owner != "" && owner != session.Owner {
		return
	}
	switch change.MustGet("type") {
	case "snapshot":
		state, _ := change.MustGet("conversationState").(*jsoncompat.Object)
		if state == nil || state.MustGet("id") != id {
			session.Error = "桌面快照与会话不匹配"
			session.Connected = false
			break
		}
		session.State = jsoncompat.Clone(state)
		session.Revision = change.MustGet("revision")
		session.Connected = true
		session.Error = ""
	case "patches":
		if session.State == nil || !jsonEqual(session.Revision, change.MustGet("baseRevision")) {
			session.Error = "同步状态发生变化，正在重新读取桌面快照"
			session.Connected = false
			break
		}
		patched, err := model.ApplyPatches(session.State, array(change.MustGet("patches")))
		if err != nil {
			session.Error = err.Error()
			session.Connected = false
			break
		}
		session.State = patched
		session.Revision = change.MustGet("revision")
		session.Connected = true
	}
	session.Sequence++
	close(session.Changed)
	session.Changed = make(chan struct{})
}

func (b *Bridge) handleDisconnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, session := range b.sessions {
		session.Connected = false
		session.Error = "桌面会话连接已断开，正在等待重连"
		session.Sequence++
		close(session.Changed)
		session.Changed = make(chan struct{})
	}
}

func (b *Bridge) Close() { b.IPC.Close() }

func object(value map[string]any) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for key, item := range value {
		result.Set(key, item)
	}
	return result
}

func array(value any) *jsoncompat.Array {
	result, _ := value.(*jsoncompat.Array)
	if result == nil {
		return &jsoncompat.Array{}
	}
	return result
}

func jsonEqual(left, right any) bool {
	return string(jsoncompat.EncodeCompact(left)) == string(jsoncompat.EncodeCompact(right))
}

var _ = errors.New
