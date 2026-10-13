// Package acpruntime owns long-lived ACP child processes and exposes their
// normalized acpview snapshots to the Go service layer.
package acpruntime

import (
	"errors"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type Session struct {
	ID      string
	Cwd     string
	Process *acp.Process
	View    *acpview.View

	mu            sync.Mutex
	promptRunning bool
	pending       map[string]*jsoncompat.Object
	pendingIDs    map[string]any
	changed       chan struct{}
	closed        bool
}

type Manager struct {
	Executable string
	mu         sync.Mutex
	sessions   map[string]*Session
	now        func() float64
}

func New(executable string) *Manager {
	return &Manager{Executable: executable, sessions: map[string]*Session{}, now: func() float64 { return float64(time.Now().UnixMilli()) / 1000 }}
}

func (m *Manager) Open(id, cwd string, timeout time.Duration) (*Session, error) {
	m.mu.Lock()
	if session := m.sessions[id]; session != nil && !session.closed {
		m.mu.Unlock()
		return session, nil
	}
	m.mu.Unlock()
	if m.Executable == "" {
		return nil, errors.New("未找到 Agent 命令，请先安装")
	}
	process, err := acp.Start([]string{m.Executable, "acp"})
	if err != nil {
		return nil, err
	}
	session := &Session{ID: id, Cwd: cwd, Process: process,
		View: acpview.NewView(id, nil, cwd, m.now), pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, changed: make(chan struct{})}
	process.OnNotification = func(message *jsoncompat.Object) { session.onNotification(message) }
	process.OnRequest = func(message *jsoncompat.Object, respond func(any)) { session.onRequest(message, respond) }
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	if _, err := process.Initialize(timeout); err != nil {
		session.Close()
		return nil, err
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", id)
	params.Set("cwd", cwd)
	params.Set("mcpServers", &jsoncompat.Array{})
	_, err = process.Request("session/load", params, timeout)
	if err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

// Create is the lifecycle entry for a new ACP conversation. Providers that
// support session/load use Open for existing ids; creation keeps the same
// process ownership and sends session/new.
func (m *Manager) Create(id, cwd string, timeout time.Duration) (*Session, error) {
	if m.Executable == "" {
		return nil, errors.New("未找到 Agent 命令，请先安装")
	}
	process, err := acp.Start([]string{m.Executable, "acp"})
	if err != nil {
		return nil, err
	}
	session := &Session{ID: id, Cwd: cwd, Process: process, View: acpview.NewView(id, nil, cwd, m.now), pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, changed: make(chan struct{})}
	process.OnNotification = func(message *jsoncompat.Object) { session.onNotification(message) }
	process.OnRequest = func(message *jsoncompat.Object, respond func(any)) { session.onRequest(message, respond) }
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	if _, err = process.Initialize(timeout); err != nil {
		session.Close()
		return nil, err
	}
	params := jsoncompat.NewObject()
	params.Set("cwd", cwd)
	params.Set("mcpServers", &jsoncompat.Array{})
	if _, err = process.Request("session/new", params, timeout); err != nil {
		session.Close()
		return nil, err
	}
	return session, nil
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *Manager) Close() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}

func (s *Session) Prompt(text string, timeout time.Duration) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.promptRunning {
		s.mu.Unlock()
		return errors.New("当前已有任务正在运行")
	}
	s.promptRunning = true
	s.View.BeginTurn(text)
	s.mu.Unlock()
	block := jsoncompat.NewObject()
	block.Set("type", "text")
	block.Set("text", text)
	params := jsoncompat.NewObject()
	params.Set("sessionId", s.ID)
	params.Set("prompt", &jsoncompat.Array{Items: []any{block}})
	go func() {
		result, err := s.Process.Request("session/prompt", params, timeout)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.promptRunning = false
		if err != nil {
			s.View.FailTurn(err.Error())
		} else {
			stopReason := "end_turn"
			if object, ok := result.(*jsoncompat.Object); ok {
				if value, ok := object.MustGet("stopReason").(string); ok && value != "" {
					stopReason = value
				}
			}
			s.View.EndTurn(stopReason)
		}
		s.signalLocked()
	}()
	return nil
}

func (s *Session) Cancel() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	s.promptRunning = false
	s.View.EndTurn("cancelled")
	s.signalLocked()
	s.mu.Unlock()
	return s.Process.Notify("session/cancel", sessionParams(s.ID))
}

func (s *Session) Respond(requestID string, response *jsoncompat.Object) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	rawID, exists := s.pendingIDs[requestID]
	if _, ok := s.pending[requestID]; !ok || !exists {
		s.mu.Unlock()
		return errors.New("请求已过期，请刷新聊天")
	}
	delete(s.pending, requestID)
	delete(s.pendingIDs, requestID)
	s.View.RemovePermission(requestID)
	s.signalLocked()
	s.mu.Unlock()
	return s.Process.Respond(rawID, response)
}

func (s *Session) Snapshot() *jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.View.Snapshot()
}

func (s *Session) Wait(timeout time.Duration) {
	s.mu.Lock()
	changed := s.changed
	s.mu.Unlock()
	select {
	case <-changed:
	case <-time.After(timeout):
	}
}

func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	process := s.Process
	s.mu.Unlock()
	process.Close()
}

func (s *Session) onNotification(message *jsoncompat.Object) {
	method, _ := message.MustGet("method").(string)
	if method != "session/update" {
		return
	}
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		return
	}
	if update, ok := params.MustGet("update").(*jsoncompat.Object); ok {
		params = update
	}
	s.mu.Lock()
	s.View.OnUpdate(params)
	s.signalLocked()
	s.mu.Unlock()
}

func (s *Session) onRequest(message *jsoncompat.Object, respond func(any)) {
	method, _ := message.MustGet("method").(string)
	requestID := jsoncompat.PyStr(message.MustGet("id"))
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		params = jsoncompat.NewObject()
	}
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]*jsoncompat.Object{}
	}
	if s.pendingIDs == nil {
		s.pendingIDs = map[string]any{}
	}
	s.pending[requestID] = params
	s.pendingIDs[requestID] = message.MustGet("id")
	switch method {
	case "session/request_permission", "session/requestPermission":
		s.View.AddPermission(requestID, params)
	case "elicitation/create":
		s.View.AddInput(requestID, params)
	default:
		delete(s.pending, requestID)
		delete(s.pendingIDs, requestID)
		s.mu.Unlock()
		respond(nil)
		return
	}
	s.signalLocked()
	s.mu.Unlock()
}

func (s *Session) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func sessionParams(id string) *jsoncompat.Object {
	params := jsoncompat.NewObject()
	params.Set("sessionId", id)
	return params
}
