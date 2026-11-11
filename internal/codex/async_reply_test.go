package codex

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/ipc"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/google/uuid"
)

type replyTestOwner struct {
	mu         sync.Mutex
	operations []*jsoncompat.Object
	fail       bool
}

func replyTestBridge(t *testing.T, status string) (*Bridge, *replyTestOwner, *jsoncompat.Object) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix IPC fixture")
	}
	path := filepath.Join(os.TempDir(), "mb-reply-"+uuid.NewString()+".sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	owner := &replyTestOwner{}
	var connection net.Conn
	var connectionMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		connectionMu.Lock()
		connection = conn
		connectionMu.Unlock()
		defer conn.Close()
		for {
			value, err := ipc.DecodeFrame(conn)
			if err != nil {
				return
			}
			request, _ := value.(*jsoncompat.Object)
			if request == nil || request.MustGet("type") != "request" {
				continue
			}
			result := jsoncompat.NewObject()
			failed := false
			if request.MustGet("method") == "initialize" {
				result.Set("clientId", "mobile-test")
			} else {
				owner.mu.Lock()
				owner.operations = append(owner.operations, request)
				failed = owner.fail
				owner.mu.Unlock()
				result.Set("ok", true)
			}
			response := jsoncompat.NewObject()
			response.Set("type", "response")
			response.Set("requestId", request.MustGet("requestId"))
			response.Set("resultType", "success")
			response.Set("result", result)
			if failed {
				response.Set("resultType", "error")
				response.Set("error", "desktop disconnected after request")
			}
			frame, _ := ipc.EncodeFrame(response)
			if _, err = conn.Write(frame); err != nil {
				return
			}
		}
	}()
	bridge := New(t.TempDir(), path)
	bridge.AsyncReplyPath = filepath.Join(t.TempDir(), "question-replies.json")
	state := replyTestState(status)
	session := bridge.session("thread-reply")
	bridge.mu.Lock()
	session.State = state
	session.Connected = true
	session.Owner = "test-owner"
	bridge.mu.Unlock()
	t.Cleanup(func() {
		bridge.Close()
		listener.Close()
		connectionMu.Lock()
		if connection != nil {
			connection.Close()
		}
		connectionMu.Unlock()
		<-done
	})
	return bridge, owner, state
}

func replyTestState(status string) *jsoncompat.Object {
	question := object(map[string]any{"title": "电脑浏览器里的 Mobile Bridge，还是桌面 App？", "options": &jsoncompat.Array{Items: []any{"电脑浏览器里的 Mobile Bridge", "桌面 App"}}})
	message := object(map[string]any{"id": "question-message", "type": "agentMessage", "questions": &jsoncompat.Array{Items: []any{question}}})
	turn := object(map[string]any{"turnId": "source-turn", "status": status, "items": &jsoncompat.Array{Items: []any{message}}})
	return object(map[string]any{"id": "thread-reply", "turns": &jsoncompat.Array{Items: []any{turn}}})
}

func pendingQuestion(state *jsoncompat.Object) (*jsoncompat.Object, string, *jsoncompat.Object) {
	pending := model.AsyncRequests(state).Items[0].(*jsoncompat.Object)
	params := pending.MustGet("params").(*jsoncompat.Object)
	question := params.MustGet("questions").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	key := question.MustGet("id").(string)
	response := object(map[string]any{"answers": object(map[string]any{key: &jsoncompat.Array{Items: []any{"电脑浏览器里的 Mobile Bridge"}}})})
	return pending, pending.MustGet("id").(string), response
}

func TestAsyncQuestionRespondUsesDesktopEnvelopeAndCorrectTurnOperation(t *testing.T) {
	for _, scenario := range []struct{ status, method string }{{"inProgress", "thread-follower-steer-turn"}, {"completed", "thread-follower-start-turn"}} {
		t.Run(scenario.status, func(t *testing.T) {
			bridge, owner, state := replyTestBridge(t, scenario.status)
			pending, requestID, response := pendingQuestion(state)
			result, err := bridge.Respond("thread-reply", requestID, response)
			if err != nil || result.MustGet("ok") != true {
				t.Fatalf("异步答复应成功: %v %v", result, err)
			}
			owner.mu.Lock()
			operations := append([]*jsoncompat.Object(nil), owner.operations...)
			owner.mu.Unlock()
			if len(operations) != 1 || operations[0].MustGet("method") != scenario.method {
				t.Fatalf("答复路径错误: %v", operations)
			}
			params := operations[0].MustGet("params").(*jsoncompat.Object)
			if scenario.status == "completed" {
				params = params.MustGet("turnStart").(*jsoncompat.Object).MustGet("request").(*jsoncompat.Object)
			}
			text := params.MustGet("input").(*jsoncompat.Array).Items[0].(*jsoncompat.Object).MustGet("text").(string)
			replies := model.QuestionReplies(text)
			if len(replies) != 1 || replies[0].MustGet("answer") != "电脑浏览器里的 Mobile Bridge" {
				t.Fatalf("答复协议错误: %s", text)
			}
			question := pending.MustGet("params").(*jsoncompat.Object).MustGet("questions").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
			if replies[0].MustGet("questionItemId") != question.MustGet("id") || replies[0].MustGet("question") != question.MustGet("question") {
				t.Fatal("答复没有绑定原始问题")
			}
			if _, err = bridge.Respond("thread-reply", requestID, response); err != nil {
				t.Fatal(err)
			}
			owner.mu.Lock()
			count := len(owner.operations)
			owner.mu.Unlock()
			if count != 1 {
				t.Fatal("重复提交不能重复发送")
			}
		})
	}
}

func TestAsyncQuestionRejectsInvalidExpiredAndUnsupportedReplies(t *testing.T) {
	bridge, owner, state := replyTestBridge(t, "inProgress")
	_, requestID, response := pendingQuestion(state)
	for _, invalid := range []*jsoncompat.Object{jsoncompat.NewObject(), object(map[string]any{"answers": object(map[string]any{"wrong-question": &jsoncompat.Array{Items: []any{"answer"}}})})} {
		if _, err := bridge.Respond("thread-reply", requestID, invalid); err == nil {
			t.Fatal("无效答案不能发送")
		}
	}
	if _, err := bridge.Respond("thread-reply", "async:old:question", response); err == nil {
		t.Fatal("已失效问题不能发送")
	}
	bridge.mu.Lock()
	state.Set("requests", &jsoncompat.Array{Items: []any{object(map[string]any{
		"id": "verification", "method": "item/tool/requestUserInput",
		"params": object(map[string]any{"openai/userVerification": true}),
	})}})
	bridge.mu.Unlock()
	if _, err := bridge.Respond("thread-reply", "verification", response); err == nil || !strings.Contains(err.Error(), "桌面 App") {
		t.Fatalf("身份验证请求仍须在桌面处理: %v", err)
	}
	owner.mu.Lock()
	count := len(owner.operations)
	owner.mu.Unlock()
	if count != 0 {
		t.Fatal("校验失败不应调用桌面写入")
	}
}

func TestAsyncQuestionConcurrentSubmissionIsSentOnce(t *testing.T) {
	bridge, owner, state := replyTestBridge(t, "inProgress")
	_, requestID, response := pendingQuestion(state)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := bridge.Respond("thread-reply", requestID, response); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	owner.mu.Lock()
	count := len(owner.operations)
	owner.mu.Unlock()
	if count != 1 {
		t.Fatalf("并发重复发送 %d 次", count)
	}
	different := object(map[string]any{"answers": object(map[string]any{"new": &jsoncompat.Array{Items: []any{"different"}}})})
	if _, err := bridge.Respond("thread-reply", requestID, different); err == nil {
		t.Fatal("同一问题不能再发不同答案")
	}
}

func TestAsyncQuestionUncertainResponseDoesNotResend(t *testing.T) {
	bridge, owner, state := replyTestBridge(t, "inProgress")
	_, requestID, response := pendingQuestion(state)
	owner.mu.Lock()
	owner.fail = true
	owner.mu.Unlock()
	if _, err := bridge.Respond("thread-reply", requestID, response); err == nil || !strings.Contains(err.Error(), "未自动重发") {
		t.Fatalf("结果未确认应有明确提示: %v", err)
	}
	if _, err := bridge.Respond("thread-reply", requestID, response); err == nil {
		t.Fatal("未知结果不能转为成功")
	}
	owner.mu.Lock()
	count := len(owner.operations)
	owner.mu.Unlock()
	if count != 1 {
		t.Fatal("未知结果不能重发")
	}
}

func TestNativeInputResponsePreservesNestedResponseShape(t *testing.T) {
	for _, scenario := range []struct {
		method, operation string
		params, response  *jsoncompat.Object
	}{
		{"item/tool/requestUserInput", "thread-follower-submit-user-input", jsoncompat.NewObject(), object(map[string]any{"answers": object(map[string]any{"q": &jsoncompat.Array{Items: []any{"answer"}}})})},
		{"tool/requestUserInput", "thread-follower-submit-user-input", jsoncompat.NewObject(), object(map[string]any{"answers": jsoncompat.NewObject()})},
		{"item/permissions/requestApproval", "thread-follower-permissions-request-approval-response", jsoncompat.NewObject(), object(map[string]any{"permissions": jsoncompat.NewObject(), "scope": "turn"})},
		{"mcpServer/elicitation/request", "thread-follower-submit-mcp-server-elicitation-response", object(map[string]any{"requestedSchema": object(map[string]any{"type": "object"})}), object(map[string]any{"action": "accept", "content": jsoncompat.NewObject()})},
	} {
		t.Run(scenario.method, func(t *testing.T) {
			bridge, owner, state := replyTestBridge(t, "inProgress")
			request := object(map[string]any{"id": "native-question", "method": scenario.method, "params": scenario.params})
			bridge.mu.Lock()
			state.Set("requests", &jsoncompat.Array{Items: []any{request}})
			bridge.mu.Unlock()
			if _, err := bridge.Respond("thread-reply", "native-question", scenario.response); err != nil {
				t.Fatal(err)
			}
			owner.mu.Lock()
			operation := owner.operations[0]
			owner.mu.Unlock()
			params := operation.MustGet("params").(*jsoncompat.Object)
			if operation.MustGet("method") != scenario.operation || params.MustGet("requestId") != "native-question" || string(jsoncompat.EncodeCompact(params.MustGet("response"))) != string(jsoncompat.EncodeCompact(scenario.response)) {
				t.Fatalf("原生 RPC 必须保留嵌套 response: %s", jsoncompat.EncodeCompact(operation))
			}
			for _, key := range scenario.response.Keys() {
				if _, exists := params.Get(key); exists {
					t.Fatalf("response 字段不能摊平到 RPC 参数: %s", jsoncompat.EncodeCompact(operation))
				}
			}
		})
	}
}

func TestAsyncQuestionReceiptsSurviveGatewayRestart(t *testing.T) {
	bridge, owner, state := replyTestBridge(t, "inProgress")
	_, requestID, response := pendingQuestion(state)
	if _, err := bridge.Respond("thread-reply", requestID, response); err != nil {
		t.Fatal(err)
	}
	restarted := New(t.TempDir(), "unused")
	restarted.AsyncReplyPath = bridge.AsyncReplyPath
	if result, err := restarted.Respond("thread-reply", requestID, response); err != nil || result == nil {
		t.Fatalf("已接收收据应幂等返回: %v %v", result, err)
	}
	owner.mu.Lock()
	count := len(owner.operations)
	owner.mu.Unlock()
	if count != 1 {
		t.Fatal("重启后不能重复发送")
	}
	// 模拟写入已经开始、桌面响应尚未持久化时网关退出。
	raw, err := os.ReadFile(bridge.AsyncReplyPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipts map[string]asyncReplyReceipt
	if err = json.Unmarshal(raw, &receipts); err != nil {
		t.Fatal(err)
	}
	for key, receipt := range receipts {
		receipt.Status = "pending"
		receipt.Result = nil
		receipts[key] = receipt
	}
	raw, _ = json.Marshal(receipts)
	if err = os.WriteFile(bridge.AsyncReplyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	interrupted := New(t.TempDir(), "unused")
	interrupted.AsyncReplyPath = bridge.AsyncReplyPath
	if _, err = interrupted.Respond("thread-reply", requestID, response); err == nil || !strings.Contains(err.Error(), "未自动重发") {
		t.Fatalf("未决收据应阻止重发: %v", err)
	}
	if strings.Contains(string(raw), "电脑浏览器里的 Mobile Bridge") {
		t.Fatal("收据不能保存答案原文")
	}
}

func TestAsyncQuestionReceiptFailureDoesNotDispatch(t *testing.T) {
	bridge, owner, state := replyTestBridge(t, "inProgress")
	_, requestID, response := pendingQuestion(state)
	bridge.AsyncReplyPath = filepath.Join(t.TempDir(), "missing", "question-replies.json")
	if _, err := bridge.Respond("thread-reply", requestID, response); err == nil {
		t.Fatal("提交前记录无法落盘时应拒绝发送")
	}
	owner.mu.Lock()
	count := len(owner.operations)
	owner.mu.Unlock()
	if count != 0 {
		t.Fatal("收据落盘失败仍发送了答案")
	}
}

func TestAsyncQuestionAnsweredSteerAcknowledgementRemovesPendingCard(t *testing.T) {
	state := replyTestState("inProgress")
	pending, _, response := pendingQuestion(state)
	text, err := asyncQuestionReplyText(pending, response)
	if err != nil {
		t.Fatal(err)
	}
	turn := state.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	turn.MustGet("items").(*jsoncompat.Array).Items = append(turn.MustGet("items").(*jsoncompat.Array).Items,
		object(map[string]any{"type": "steeringUserMessage", "status": "inProgress", "steerAcknowledged": true, "input": &jsoncompat.Array{Items: []any{object(map[string]any{"type": "text", "text": text})}}}))
	if len(model.AsyncRequests(state).Items) != 0 {
		t.Fatal("桌面已确认的补充消息不应留下可提交卡片")
	}
}
