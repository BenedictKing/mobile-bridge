// Package acp is the Go port of bridge/agents/acp.py: the ACP stdio
// transport (NDJSON JSON-RPC 2.0 over pipes). The wire contract — default
// Python separators, sequential integer ids, banner tolerance, reverse
// requests, the -32000 auth code — is enforced against conformance/acp
// goldens produced by driving both implementations against the same fake
// agent process.
package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// One NDJSON frame may never exceed 64MB; a larger line breaks the framing.
const MaxLine = 64 * 1024 * 1024

const authRequiredCode = -32000

// ErrorKind mirrors the Python exception subclass names.
type ErrorKind string

const (
	KindError        ErrorKind = "AcpError"
	KindUnavailable  ErrorKind = "AcpUnavailable"
	KindTimeout      ErrorKind = "AcpTimeout"
	KindProtocol     ErrorKind = "AcpProtocol"
	KindAuthRequired ErrorKind = "AcpAuthRequired"
)

// Error mirrors AcpError; JSON-RPC error objects keep their code and data.
type Error struct {
	Message string
	Code    any
	Data    any
	Kind    ErrorKind
}

func (e *Error) Error() string { return e.Message }

func unavailable() *Error {
	return &Error{Message: "Agent 进程已退出；操作结果未知", Kind: KindUnavailable}
}

type delivery struct {
	value any
	err   *Error
}

// Process mirrors AcpProcess: one ACP agent subprocess with serialized
// writes and paired waiters.
type Process struct {
	OnNotification func(*jsoncompat.Object)
	OnRequest      func(message *jsoncompat.Object, respond func(any))

	Capabilities *jsoncompat.Object
	AgentInfo    *jsoncompat.Object
	AuthMethods  *jsoncompat.Array

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu      sync.Mutex
	pending map[string]chan delivery
	nextID  int
	closed  bool
	writeMu sync.Mutex
	done    chan struct{}
}

// Start launches the agent process and its reader loop.
func Start(argv []string) (*Process, error) {
	process := &Process{pending: map[string]chan delivery{}, done: make(chan struct{})}
	process.cmd = exec.Command(argv[0], argv[1:]...)
	stdin, err := process.cmd.StdinPipe()
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("无法启动 Agent 进程：%s", err), Kind: KindUnavailable}
	}
	stdout, err := process.cmd.StdoutPipe()
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("无法启动 Agent 进程：%s", err), Kind: KindUnavailable}
	}
	process.stdin, process.stdout = stdin, stdout
	stderr, err := process.cmd.StderrPipe()
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("无法启动 Agent 进程：%s", err), Kind: KindUnavailable}
	}
	if err := process.cmd.Start(); err != nil {
		return nil, &Error{Message: fmt.Sprintf("无法启动 Agent 进程：%s", err), Kind: KindUnavailable}
	}
	// Drain stderr so a chatty agent cannot block.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go process.readLoop()
	return process, nil
}

func (p *Process) send(message *jsoncompat.Object) error {
	data := append(jsoncompat.EncodePythonDefault(message), '\n')
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.closed {
		return unavailable()
	}
	if _, err := p.stdin.Write(data); err != nil {
		return unavailable()
	}
	return nil
}

// Submit mirrors submit: register and send; returns the id lexeme and a
// channel delivering the outcome.
func (p *Process) Submit(method string, params *jsoncompat.Object) (string, <-chan delivery, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return "", nil, unavailable()
	}
	ident := jsoncompat.Num(p.nextID)
	p.nextID++
	channel := make(chan delivery, 1)
	p.pending[ident.String()] = channel
	p.mu.Unlock()
	message := jsoncompat.NewObject()
	message.Set("jsonrpc", "2.0")
	message.Set("id", ident)
	message.Set("method", method)
	message.Set("params", params)
	if err := p.send(message); err != nil {
		p.mu.Lock()
		delete(p.pending, ident.String())
		p.mu.Unlock()
		return "", nil, err
	}
	return ident.String(), channel, nil
}

// Request mirrors request: send and wait with a timeout.
func (p *Process) Request(method string, params *jsoncompat.Object, timeout time.Duration) (any, error) {
	_, channel, err := p.Submit(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case outcome := <-channel:
		if outcome.err != nil {
			return nil, outcome.err
		}
		return outcome.value, nil
	case <-time.After(timeout):
		return nil, &Error{Message: "Agent 请求超时：" + method, Kind: KindTimeout}
	}
}

// Initialize mirrors the ACP handshake and captures agent metadata.
func (p *Process) Initialize(timeout time.Duration) (any, error) {
	fs := jsoncompat.NewObject()
	fs.Set("readTextFile", false)
	fs.Set("writeTextFile", false)
	capabilities := jsoncompat.NewObject()
	capabilities.Set("fs", fs)
	capabilities.Set("terminal", false)
	elicitation := jsoncompat.NewObject()
	elicitation.Set("form", true)
	capabilities.Set("elicitation", elicitation)
	params := jsoncompat.NewObject()
	params.Set("protocolVersion", jsoncompat.Num(1))
	params.Set("clientCapabilities", capabilities)
	result, err := p.Request("initialize", params, timeout)
	if err != nil {
		return nil, err
	}
	resultObject, _ := result.(*jsoncompat.Object)
	if resultObject == nil {
		resultObject = jsoncompat.NewObject()
	}
	p.Capabilities, _ = truthyOr(resultObject.MustGet("agentCapabilities"),
		any(jsoncompat.NewObject())).(*jsoncompat.Object)
	p.AgentInfo, _ = truthyOr(resultObject.MustGet("agentInfo"),
		any(jsoncompat.NewObject())).(*jsoncompat.Object)
	if methods := arrOf(resultObject.MustGet("authMethods")); methods != nil {
		p.AuthMethods = methods
	} else {
		p.AuthMethods = &jsoncompat.Array{}
	}
	return result, nil
}

func truthyOr(value, fallback any) any {
	if jsoncompat.Truthy(value) {
		return value
	}
	return fallback
}

func arrOf(value any) *jsoncompat.Array {
	array, _ := value.(*jsoncompat.Array)
	return array
}

// Notify mirrors notify: no id, no response expected.
func (p *Process) Notify(method string, params *jsoncompat.Object) error {
	message := jsoncompat.NewObject()
	message.Set("jsonrpc", "2.0")
	message.Set("method", method)
	message.Set("params", params)
	return p.send(message)
}

// Respond mirrors respond: answer one reverse request.
func (p *Process) Respond(requestID any, result any) error {
	message := jsoncompat.NewObject()
	message.Set("jsonrpc", "2.0")
	message.Set("id", requestID)
	message.Set("result", result)
	return p.send(message)
}

// RespondError mirrors respond_error.
func (p *Process) RespondError(requestID any, code int, message string) error {
	failure := jsoncompat.NewObject()
	failure.Set("code", jsoncompat.Num(code))
	failure.Set("message", message)
	envelope := jsoncompat.NewObject()
	envelope.Set("jsonrpc", "2.0")
	envelope.Set("id", requestID)
	envelope.Set("error", failure)
	return p.send(envelope)
}

// Close mirrors close: stdin EOF, terminate, wait, then kill.
func (p *Process) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
		}
	}
	p.failAll(unavailable())
	close(p.done)
	_ = p.stdout.Close()
}

func (p *Process) failAll(failure *Error) {
	p.mu.Lock()
	pending := p.pending
	p.pending = map[string]chan delivery{}
	p.mu.Unlock()
	for _, channel := range pending {
		channel <- delivery{err: failure}
	}
}

func (p *Process) readLoop() {
	reader := bufio.NewReaderSize(p.stdout, 64*1024)
	for {
		line, err := readLine(reader)
		if err != nil {
			return
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		value, parseErr := jsoncompat.Unmarshal([]byte(trimmed))
		if parseErr != nil {
			// Real agents print banners; one bad line never kills the stream.
			continue
		}
		message, _ := value.(*jsoncompat.Object)
		if message == nil {
			continue
		}
		p.dispatch(message)
	}
}

// readLine mirrors readline semantics without bufio.Scanner's token cap.
func readLine(reader *bufio.Reader) (string, error) {
	var builder strings.Builder
	for {
		chunk, isPrefix, err := reader.ReadLine()
		builder.Write(chunk)
		if err != nil {
			return builder.String(), err
		}
		if !isPrefix {
			return builder.String(), nil
		}
	}
}

func (p *Process) dispatch(message *jsoncompat.Object) {
	_, hasMethod := message.Get("method")
	idValue, hasID := message.Get("id")
	if hasMethod && hasID {
		if p.OnRequest == nil {
			_ = p.RespondError(idValue, -32601, "Method not found")
			return
		}
		p.OnRequest(message, func(result any) { _ = p.Respond(idValue, result) })
		return
	}
	if hasID {
		p.mu.Lock()
		channel := p.pending[idKey(idValue)]
		delete(p.pending, idKey(idValue))
		p.mu.Unlock()
		if channel == nil {
			return
		}
		if failure := objOf(message.MustGet("error")); failure != nil {
			text := "Agent 请求失败"
			if message, ok := failure.MustGet("message").(string); ok && message != "" {
				text = message
			}
			kind := KindError
			code := failure.MustGet("code")
			if number, ok := code.(json.Number); ok {
				if parsed, err := number.Float64(); err == nil && parsed == authRequiredCode {
					kind = KindAuthRequired
				}
			}
			channel <- delivery{err: &Error{Message: text, Code: code,
				Data: failure.MustGet("data"), Kind: kind}}
			return
		}
		result, _ := message.Get("result")
		channel <- delivery{value: result}
		return
	}
	if hasMethod && p.OnNotification != nil {
		p.OnNotification(message)
	}
}

func objOf(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

func idKey(value any) string {
	return string(jsoncompat.EncodeCompact(value))
}

var _ = json.Marshal
var _ = errors.New
