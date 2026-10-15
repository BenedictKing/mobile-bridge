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
	conn        net.Conn
	initialized *jsoncompat.Object
	buffered    []*jsoncompat.Object
	nextID      int
	pending     map[string]pendingRequest
	reverse     map[string]bool
	childIn     io.WriteCloser
	childOut    io.ReadCloser
	childWrite  sync.Mutex
	connWrite   sync.Mutex
}

type pendingRequest struct {
	gatewayID any
	method    string
}

func New(socket string, command []string) *Host {
	return &Host{Socket: socket, Command: append([]string{}, command...), pending: map[string]pendingRequest{}, reverse: map[string]bool{}}
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
		conn := h.conn
		h.mu.Unlock()
		if conn != nil {
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
		h.replaceConnection(conn)
		h.flushBuffered()
		h.readGateway(conn)
		h.clearConnection(conn)
	}
}

func deadline(ctx context.Context) (deadlineTime time.Time) {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(250 * time.Millisecond)
}

func (h *Host) replaceConnection(conn net.Conn) {
	h.mu.Lock()
	old := h.conn
	h.conn = conn
	h.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (h *Host) clearConnection(conn net.Conn) {
	h.mu.Lock()
	if h.conn == conn {
		h.conn = nil
	}
	h.mu.Unlock()
	_ = conn.Close()
}

func (h *Host) readGateway(conn net.Conn) {
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
			h.forwardGateway(message)
		}
	}
}

func (h *Host) forwardGateway(message *jsoncompat.Object) {
	method, _ := message.MustGet("method").(string)
	if method == "" {
		if id, ok := message.Get("id"); ok {
			key := jsoncompat.PyStr(id)
			h.mu.Lock()
			isReverse := h.reverse[key]
			delete(h.reverse, key)
			h.mu.Unlock()
			if isReverse {
				h.sendChild(message)
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
			_ = h.sendGateway(response)
			return
		}
	}
	if id, ok := message.Get("id"); ok {
		h.mu.Lock()
		h.nextID++
		hostID := fmt.Sprintf("host-%d", h.nextID)
		h.pending[hostID] = pendingRequest{gatewayID: id, method: method}
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
			h.reverse[jsoncompat.PyStr(id)] = true
			h.mu.Unlock()
			_ = method
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
			response, _ := jsoncompat.Clone(message).(*jsoncompat.Object)
			if request.method == "initialize" {
				h.mu.Lock()
				h.initialized = jsoncompat.Clone(response).(*jsoncompat.Object)
				h.mu.Unlock()
			}
			response.Set("id", request.gatewayID)
			_ = h.sendGateway(response)
			return
		}
	}
	if err := h.sendGateway(message); err != nil {
		h.mu.Lock()
		if len(h.buffered) >= maxBufferedMessages {
			copy(h.buffered, h.buffered[1:])
			h.buffered = h.buffered[:maxBufferedMessages-1]
		}
		h.buffered = append(h.buffered, jsoncompat.Clone(message).(*jsoncompat.Object))
		h.mu.Unlock()
	}
}

func (h *Host) flushBuffered() {
	h.mu.Lock()
	buffered := h.buffered
	h.buffered = nil
	h.mu.Unlock()
	for _, message := range buffered {
		_ = h.sendGateway(message)
	}
}

func (h *Host) sendChild(message *jsoncompat.Object) {
	h.childWrite.Lock()
	defer h.childWrite.Unlock()
	_, _ = h.childIn.Write(append(jsoncompat.EncodePythonDefault(message), '\n'))
}

func (h *Host) sendGateway(message *jsoncompat.Object) error {
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	if conn == nil {
		return errors.New("gateway disconnected")
	}
	h.connWrite.Lock()
	defer h.connWrite.Unlock()
	_, err := conn.Write(append(jsoncompat.EncodePythonDefault(message), '\n'))
	return err
}
