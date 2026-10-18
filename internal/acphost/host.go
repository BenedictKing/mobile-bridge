// Package acphost keeps an ACP child alive independently from the gateway.
// The gateway connects through one Unix socket and may disconnect/reconnect
// without closing the child's stdio transport.
package acphost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const maxBufferedMessages = 256

type Host struct {
	Socket  string
	Command []string

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	lastConn    net.Conn
	sessions    map[string]net.Conn
	initialized *jsoncompat.Object
	buffered    []*jsoncompat.Object
	nextID      int
	pending     map[string]pendingRequest
	reverse     map[string]net.Conn
	childIn     io.WriteCloser
	childOut    io.ReadCloser
	childWrite  sync.Mutex
	connWrite   sync.Mutex
}

type pendingRequest struct {
	gatewayID any
	method    string
	conn      net.Conn
}

func New(socket string, command []string) *Host {
	return &Host{Socket: socket, Command: append([]string{}, command...), connections: map[net.Conn]struct{}{}, sessions: map[string]net.Conn{}, pending: map[string]pendingRequest{}, reverse: map[string]net.Conn{}}
}

// Run starts the child and accepts gateway connections until ctx is canceled.
func (h *Host) Run(ctx context.Context) error {
	if len(h.Command) == 0 {
		return errors.New("Agent Host 未配置 Agent 命令")
	}
	if err := os.MkdirAll(filepath.Dir(h.Socket), 0o700); err != nil {
		return err
	}
	_ = os.Remove(h.Socket)
	listener, err := net.Listen("unix", h.Socket)
	if err != nil {
		return fmt.Errorf("监听 Agent Host：%w", err)
	}
	defer func() { _ = listener.Close(); _ = os.Remove(h.Socket) }()
	_ = os.Chmod(h.Socket, 0o600)

	command := exec.Command(h.Command[0], h.Command[1:]...)
	h.childIn, err = command.StdinPipe()
	if err != nil {
		return err
	}
	h.childOut, err = command.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	childDone := make(chan error, 1)
	go func() { childDone <- h.readChild() }()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		connections := make([]net.Conn, 0, len(h.connections))
		for conn := range h.connections {
			connections = append(connections, conn)
		}
		h.mu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		_ = h.childIn.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}()

	for {
		listener.(*net.UnixListener).SetDeadline(deadline(ctx))
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			select {
			case <-ctx.Done():
				_ = h.childIn.Close()
				_ = command.Process.Kill()
				<-childDone
				return nil
			case childErr := <-childDone:
				return childErr
			default:
			}
			if ne, ok := acceptErr.(net.Error); ok && ne.Timeout() {
				continue
			}
			return acceptErr
		}
		h.addConnection(conn)
		h.flushBuffered(conn)
		go h.readGateway(conn)
	}
}

func deadline(ctx context.Context) (deadlineTime time.Time) {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(250 * time.Millisecond)
}

func (h *Host) addConnection(conn net.Conn) {
	h.mu.Lock()
	h.connections[conn] = struct{}{}
	h.lastConn = conn
	h.mu.Unlock()
}

func (h *Host) clearConnection(conn net.Conn) {
	h.mu.Lock()
	delete(h.connections, conn)
	if h.lastConn == conn {
		h.lastConn = nil
	}
	for sessionID, sessionConn := range h.sessions {
		if sessionConn == conn {
			delete(h.sessions, sessionID)
		}
	}
	h.mu.Unlock()
	_ = conn.Close()
}

func (h *Host) readGateway(conn net.Conn) {
	defer h.clearConnection(conn)
	reader := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		value, err := jsoncompat.Unmarshal([]byte(strings.TrimSpace(line)))
		if err != nil {
			continue
		}
		message, ok := value.(*jsoncompat.Object)
		if ok {
			h.forwardGateway(conn, message)
		}
	}
}

func (h *Host) forwardGateway(conn net.Conn, message *jsoncompat.Object) {
	method, _ := message.MustGet("method").(string)
	if params, ok := message.MustGet("params").(*jsoncompat.Object); ok {
		if sessionID, ok := params.MustGet("sessionId").(string); ok && sessionID != "" {
			h.mu.Lock()
			h.sessions[sessionID] = conn
			h.mu.Unlock()
		}
	}
	if method == "" {
		if id, ok := message.Get("id"); ok {
			key := jsoncompat.PyStr(id)
			h.mu.Lock()
			reverseConn, isReverse := h.reverse[key]
			delete(h.reverse, key)
			h.mu.Unlock()
			if isReverse {
				if reverseConn == conn {
					h.sendChild(message)
				}
				return
			}
		}
	}
	if method == "initialize" {
		h.mu.Lock()
		var cached any
		if h.initialized != nil {
			cached = jsoncompat.Clone(h.initialized)
		}
		h.mu.Unlock()
		if cached != nil {
			response := cached.(*jsoncompat.Object)
			response.Set("id", message.MustGet("id"))
			_ = h.sendGateway(conn, response)
			return
		}
	}
	if id, ok := message.Get("id"); ok {
		h.mu.Lock()
		h.nextID++
		hostID := fmt.Sprintf("host-%d", h.nextID)
		h.pending[hostID] = pendingRequest{gatewayID: id, method: method, conn: conn}
		h.mu.Unlock()
		forwarded, _ := jsoncompat.Clone(message).(*jsoncompat.Object)
		forwarded.Set("id", hostID)
		h.sendChild(forwarded)
		return
	}
	h.sendChild(message)
}

func (h *Host) readChild() error {
	reader := bufio.NewReaderSize(h.childOut, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		value, err := jsoncompat.Unmarshal([]byte(strings.TrimSpace(line)))
		if err != nil {
			continue
		}
		message, ok := value.(*jsoncompat.Object)
		if ok {
			h.forwardChild(message)
		}
	}
}

func (h *Host) forwardChild(message *jsoncompat.Object) {
	if method, hasMethod := message.Get("method"); hasMethod {
		if id, hasID := message.Get("id"); hasID {
			h.mu.Lock()
			conn := h.lastConn
			if params, ok := message.MustGet("params").(*jsoncompat.Object); ok {
				if sessionID, ok := params.MustGet("sessionId").(string); ok && sessionID != "" {
					if sessionConn := h.sessions[sessionID]; sessionConn != nil {
						conn = sessionConn
					}
				}
			}
			h.reverse[jsoncompat.PyStr(id)] = conn
			h.mu.Unlock()
			if conn != nil {
				_ = h.sendGateway(conn, message)
			}
			_ = method
			return
		}
		// A session-scoped notification goes only to the connection that owns
		// the session; broadcasting it would fold one chat's stream into every
		// other open chat. Unknown sessions keep the broadcast fallback — the
		// gateway side filters by sessionId.
		if params, ok := message.MustGet("params").(*jsoncompat.Object); ok {
			if sessionID, ok := params.MustGet("sessionId").(string); ok && sessionID != "" {
				h.mu.Lock()
				conn := h.sessions[sessionID]
				h.mu.Unlock()
				if conn != nil {
					_ = h.sendGateway(conn, message)
					return
				}
			}
		}
	}
	if id, ok := message.Get("id"); ok {
		key := jsoncompat.PyStr(id)
		h.mu.Lock()
		request, pending := h.pending[key]
		if pending {
			delete(h.pending, key)
		}
		h.mu.Unlock()
		if pending {
			// session/new carries no sessionId in its params, so forwardGateway
			// never registers the fresh session; adopt the id from the result
			// or its notifications would fall back to the broadcast path.
			if request.method == "session/new" {
				if result, ok := message.MustGet("result").(*jsoncompat.Object); ok {
					if sessionID, ok := result.MustGet("sessionId").(string); ok && sessionID != "" {
						h.mu.Lock()
						h.sessions[sessionID] = request.conn
						h.mu.Unlock()
					}
				}
			}
			response, _ := jsoncompat.Clone(message).(*jsoncompat.Object)
			if request.method == "initialize" {
				h.mu.Lock()
				h.initialized = jsoncompat.Clone(response).(*jsoncompat.Object)
				h.mu.Unlock()
			}
			response.Set("id", request.gatewayID)
			_ = h.sendGateway(request.conn, response)
			return
		}
	}
	if err := h.broadcastGateway(message); err != nil {
		h.mu.Lock()
		if len(h.buffered) >= maxBufferedMessages {
			copy(h.buffered, h.buffered[1:])
			h.buffered = h.buffered[:maxBufferedMessages-1]
		}
		h.buffered = append(h.buffered, jsoncompat.Clone(message).(*jsoncompat.Object))
		h.mu.Unlock()
	}
}

func (h *Host) flushBuffered(conn net.Conn) {
	h.mu.Lock()
	buffered := h.buffered
	h.buffered = nil
	h.mu.Unlock()
	for _, message := range buffered {
		_ = h.sendGateway(conn, message)
	}
}

func (h *Host) sendChild(message *jsoncompat.Object) {
	h.childWrite.Lock()
	defer h.childWrite.Unlock()
	_, err := h.childIn.Write(append(jsoncompat.EncodePythonDefault(message), '\n'))
	_ = err
}

func (h *Host) sendGateway(conn net.Conn, message *jsoncompat.Object) error {
	if conn == nil {
		return errors.New("gateway disconnected")
	}
	h.connWrite.Lock()
	defer h.connWrite.Unlock()
	_, err := conn.Write(append(jsoncompat.EncodePythonDefault(message), '\n'))
	return err
}

func (h *Host) broadcastGateway(message *jsoncompat.Object) error {
	h.mu.Lock()
	connections := make([]net.Conn, 0, len(h.connections))
	for conn := range h.connections {
		connections = append(connections, conn)
	}
	h.mu.Unlock()
	var firstErr error
	for _, conn := range connections {
		if err := h.sendGateway(conn, message); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(connections) == 0 {
		return errors.New("gateway disconnected")
	}
	return firstErr
}
