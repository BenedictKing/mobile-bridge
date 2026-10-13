// Package ipc is the Go port of bridge/ipc.py's wire protocol: the desktop
// app's local, length-prefixed JSON framing. Message construction must stay
// byte-compatible with the Python client (key order, version bumping, compact
// separators); parity is enforced against conformance/ipc goldens.
package ipc

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const MaxFrame = 64 * 1024 * 1024

var methodVersions = map[string]int{
	"initialize":                                             0,
	"thread-owner-discovery":                                 1,
	"thread-follower-start-turn":                             2,
	"thread-follower-edit-last-user-turn":                    2,
	"thread-follower-update-thread-settings":                 2,
	"thread-follower-load-complete-history":                  1,
	"thread-follower-steer-turn":                             1,
	"thread-follower-interrupt-turn":                         4,
	"thread-follower-command-approval-decision":              1,
	"thread-follower-file-approval-decision":                 1,
	"thread-follower-permissions-request-approval-response":  1,
	"thread-follower-submit-user-input":                      1,
	"thread-follower-submit-mcp-server-elicitation-response": 1,
	"thread-stream-following-changed":                        1,
}

// ErrUnsupported mirrors IPCError("不支持的桌面操作").
var ErrUnsupported = errors.New("不支持的桌面操作")

// Client builds desktop IPC messages. NewID supplies request ids; tests
// inject the same deterministic counter the Python oracle uses.
type Client struct {
	ClientID string
	NewID    func() string
}

// BuildRequest mirrors DesktopIPC.request message construction, including the
// host-qualified version bump for thread-follower-* methods.
func (c *Client) BuildRequest(method string, params *jsoncompat.Object, target, host string, timeoutSeconds float64) (*jsoncompat.Object, error) {
	version, ok := methodVersions[method]
	if !ok {
		return nil, ErrUnsupported
	}
	if host == "" {
		host = "local"
	}
	message := jsoncompat.NewObject()
	message.Set("type", "request")
	message.Set("requestId", c.NewID())
	message.Set("sourceClientId", c.ClientID)
	message.Set("method", method)
	message.Set("version", jsoncompat.Num(version))
	message.Set("params", params)
	message.Set("timeoutMs", jsoncompat.Num(int(timeoutSeconds*1000)))
	if target != "" {
		message.Set("targetClientId", target)
	}
	if host != "local" {
		message.Set("hostId", host)
		if strings.HasPrefix(method, "thread-follower-") {
			message.Set("version", jsoncompat.Num(version+1))
		}
	}
	return message, nil
}

// BuildFollow mirrors DesktopIPC.follow's broadcast message.
func (c *Client) BuildFollow(sessionID, owner string, enabled bool, host string) *jsoncompat.Object {
	if host == "" {
		host = "local"
	}
	params := jsoncompat.NewObject()
	params.Set("hostId", host)
	params.Set("conversationId", sessionID)
	params.Set("following", enabled)
	message := jsoncompat.NewObject()
	message.Set("type", "broadcast")
	message.Set("sourceClientId", c.ClientID)
	message.Set("method", "thread-stream-following-changed")
	message.Set("version", jsoncompat.Num(1))
	message.Set("params", params)
	if owner != "" {
		targets := &jsoncompat.Array{Items: []any{owner}}
		message.Set("targetClientIds", targets)
	}
	return message
}

// BuildDiscoveryResponse mirrors the read loop's canHandle:false reply.
func BuildDiscoveryResponse(requestID string) *jsoncompat.Object {
	response := jsoncompat.NewObject()
	response.Set("canHandle", false)
	message := jsoncompat.NewObject()
	message.Set("type", "client-discovery-response")
	message.Set("requestId", requestID)
	message.Set("response", response)
	return message
}

// DispatchIncoming applies the read loop's inbound rules: answer discovery
// probes, deliver broadcasts addressed to us (or to everyone).
func DispatchIncoming(message *jsoncompat.Object, clientID string, respond func(*jsoncompat.Object), onEvent func(*jsoncompat.Object)) {
	switch kind, _ := message.MustGet("type").(string); kind {
	case "client-discovery-request":
		requestID, _ := message.MustGet("requestId").(string)
		respond(BuildDiscoveryResponse(requestID))
	case "broadcast":
		if targets, ok := message.MustGet("targetClientIds").(*jsoncompat.Array); ok && len(targets.Items) > 0 {
			for _, target := range targets.Items {
				if id, _ := target.(string); id == clientID {
					onEvent(message)
					return
				}
			}
			return
		}
		onEvent(message)
	}
}

// EncodeFrame produces the length-prefixed wire form: little-endian uint32
// byte count followed by compact JSON.
func EncodeFrame(payload any) ([]byte, error) {
	body := jsoncompat.EncodeCompact(payload)
	if len(body) > MaxFrame {
		return nil, errors.New("消息过大")
	}
	frame := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	return frame, nil
}

// DecodeFrame reads one length-prefixed frame and parses its JSON payload.
func DecodeFrame(reader io.Reader) (any, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(prefix[:])
	if length == 0 || length > MaxFrame {
		return nil, errors.New("桌面 IPC 帧长度不兼容")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	value, err := jsoncompat.Unmarshal(body)
	if err != nil {
		return nil, fmt.Errorf("ipc frame: %w", err)
	}
	return value, nil
}

// HexOf is a convenience for tests that capture frames as hex text.
func HexOf(frame []byte) string { return hex.EncodeToString(frame) }
