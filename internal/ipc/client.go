package ipc

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/transport"
	"github.com/google/uuid"
)

var errDisconnected = errors.New("桌面连接已断开；请检查会话后再决定是否重发")

// Desktop is the live length-prefixed IPC client used by the Codex owner.
// It keeps request correlation and broadcast delivery separate so a stream
// update never blocks a pending write operation.
type Desktop struct {
	Path         string
	ClientID     string
	NewID        func() string
	OnEvent      func(*jsoncompat.Object)
	OnDisconnect func()

	mu        sync.Mutex
	writeMu   sync.Mutex
	connectMu sync.Mutex
	conn      net.Conn
	pending   map[string]chan *jsoncompat.Object
	closed    bool
}

func NewDesktop(path string) *Desktop {
	return &Desktop{Path: path, NewID: func() string { return uuid.NewString() },
		OnEvent: func(*jsoncompat.Object) {}, OnDisconnect: func() {}, pending: map[string]chan *jsoncompat.Object{}}
}

func (d *Desktop) Connect(timeout time.Duration) error {
	d.connectMu.Lock()
	defer d.connectMu.Unlock()
	d.mu.Lock()
	if d.conn != nil && !d.closed {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()
	conn, err := transport.ConnectStream(d.Path, timeout)
	if err != nil {
		return errors.New("桌面 App 未连接，请打开 Codex App。")
	}
	d.mu.Lock()
	d.conn, d.closed = conn, false
	d.mu.Unlock()
	go d.readLoop(conn)
	params := jsoncompat.NewObject()
	params.Set("clientType", "codex-mobile-bridge")
	response, err := d.Request("initialize", params, "", "local", 5*time.Second)
	if err != nil {
		d.Close()
		return err
	}
	result, _ := response.MustGet("result").(*jsoncompat.Object)
	clientID, _ := result.MustGet("clientId").(string)
	if clientID == "" {
		d.Close()
		return errors.New("桌面初始化响应无效")
	}
	d.mu.Lock()
	d.ClientID = clientID
	d.mu.Unlock()
	return nil
}

func (d *Desktop) send(message *jsoncompat.Object) error {
	frame, err := EncodeFrame(message)
	if err != nil {
		return err
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.Lock()
	conn := d.conn
	closed := d.closed
	d.mu.Unlock()
	if conn == nil || closed {
		return errDisconnected
	}
	if _, err := conn.Write(frame); err != nil {
		return errDisconnected
	}
	return nil
}

// Request sends one supported desktop operation and waits for its response.
func (d *Desktop) Request(method string, params *jsoncompat.Object, target, host string, timeout time.Duration) (*jsoncompat.Object, error) {
	requestID := d.NewID()
	message, err := (&Client{ClientID: d.ClientID, NewID: func() string { return requestID }}).
		BuildRequest(method, params, target, host, timeout.Seconds())
	if err != nil {
		return nil, err
	}
	channel := make(chan *jsoncompat.Object, 1)
	d.mu.Lock()
	if d.pending == nil {
		d.pending = map[string]chan *jsoncompat.Object{}
	}
	d.pending[requestID] = channel
	d.mu.Unlock()
	if err := d.send(message); err != nil {
		d.discard(requestID)
		return nil, err
	}
	select {
	case response := <-channel:
		d.mu.Lock()
		delete(d.pending, requestID)
		d.mu.Unlock()
		if response == nil {
			return nil, errDisconnected
		}
		if response.MustGet("resultType") != "success" {
			return nil, errors.New(jsoncompat.PyStr(response.MustGet("error")))
		}
		return response, nil
	case <-time.After(timeout + time.Second):
		d.discard(requestID)
		return nil, errors.New("桌面响应超时；操作可能已提交，请查看会话后再决定是否重发")
	}
}

func (d *Desktop) discard(requestID string) {
	d.mu.Lock()
	delete(d.pending, requestID)
	d.mu.Unlock()
}

func (d *Desktop) Follow(sessionID, owner, host string, enabled bool) error {
	message := (&Client{ClientID: d.ClientID}).BuildFollow(sessionID, owner, enabled, host)
	return d.send(message)
}

func (d *Desktop) readLoop(conn net.Conn) {
	for {
		value, err := DecodeFrame(conn)
		if err != nil {
			d.disconnect(conn)
			return
		}
		message, ok := value.(*jsoncompat.Object)
		if !ok {
			continue
		}
		d.dispatch(message)
	}
}

func (d *Desktop) dispatch(message *jsoncompat.Object) {
	if message.MustGet("type") == "response" {
		requestID, _ := message.MustGet("requestId").(string)
		d.mu.Lock()
		channel := d.pending[requestID]
		d.mu.Unlock()
		if channel != nil {
			select {
			case channel <- message:
			default:
			}
		}
		return
	}
	if message.MustGet("type") == "client-discovery-request" {
		requestID, _ := message.MustGet("requestId").(string)
		_ = d.send(BuildDiscoveryResponse(requestID))
		return
	}
	if message.MustGet("type") == "broadcast" {
		DispatchIncoming(message, d.ClientID, func(*jsoncompat.Object) {}, d.OnEvent)
	}
}

func (d *Desktop) disconnect(conn net.Conn) {
	d.mu.Lock()
	if d.conn != conn {
		d.mu.Unlock()
		return
	}
	d.conn = nil
	d.closed = true
	pending := d.pending
	d.pending = map[string]chan *jsoncompat.Object{}
	d.mu.Unlock()
	_ = conn.Close()
	for _, channel := range pending {
		select {
		case channel <- nil:
		default:
		}
	}
	d.OnDisconnect()
}

func (d *Desktop) Close() {
	d.mu.Lock()
	conn := d.conn
	d.conn = nil
	d.closed = true
	pending := d.pending
	d.pending = map[string]chan *jsoncompat.Object{}
	d.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	for _, channel := range pending {
		select {
		case channel <- nil:
		default:
		}
	}
}
