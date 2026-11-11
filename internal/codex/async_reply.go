package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/google/uuid"
)

type asyncReply struct {
	fingerprint string
	done        chan struct{}
	result      *jsoncompat.Object
	err         error
	status      string
}

func asyncResponseHash(response *jsoncompat.Object) string {
	var canonical any
	_ = json.Unmarshal(jsoncompat.EncodeCompact(response), &canonical)
	raw, _ := json.Marshal(canonical)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func (b *Bridge) existingAsyncReply(id, requestID string, response *jsoncompat.Object) (*jsoncompat.Object, error, bool) {
	b.mu.Lock()
	if err := b.loadAsyncRepliesLocked(); err != nil {
		b.mu.Unlock()
		return nil, err, true
	}
	reply := b.asyncReplies[id+"\x00"+requestID]
	b.mu.Unlock()
	if reply == nil {
		return nil, nil, false
	}
	if reply.fingerprint != asyncResponseHash(response) {
		return nil, errors.New("此问题已有回应正在提交或已经提交，请刷新后查看"), true
	}
	<-reply.done
	if reply.result == nil {
		return nil, reply.err, true
	}
	copy, _ := jsoncompat.Clone(reply.result).(*jsoncompat.Object)
	return copy, reply.err, true
}

// 异步问题是 agentMessage.questions，不是原生反向 RPC。答复与桌面一样作为带
// questionItemId 的用户消息提交：当前回合在执行时 steer，已结束时启动后续回合。
func (b *Bridge) respondAsyncQuestion(id, requestID string, pending, response *jsoncompat.Object) (*jsoncompat.Object, error) {
	text, err := asyncQuestionReplyText(pending, response)
	if err != nil {
		return nil, err
	}
	// 连接失败发生在占用提交身份之前，可在恢复连接后重试。
	session, err := b.Watch(id, 1500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if owner, _ := b.ownerAndError(session); owner == "" {
		return nil, errors.New("请先在桌面 App 打开此聊天")
	}
	// Watch 可能刷新快照；只允许回应仍存在的同一问题，不把旧答案发到新回合。
	state, _, _, _ := b.Snapshot(id)
	if state == nil {
		return nil, errors.New("请求已过期，请刷新聊天")
	}
	var fresh *jsoncompat.Object
	for _, value := range model.AsyncRequests(state).Items {
		entry, _ := value.(*jsoncompat.Object)
		if entry != nil && entry.MustGet("id") == requestID {
			fresh = entry
			break
		}
	}
	if fresh == nil {
		return nil, errors.New("请求已过期，请刷新聊天")
	}
	if freshText, invalid := asyncQuestionReplyText(fresh, response); invalid != nil || freshText != text {
		return nil, errors.New("问题已更新，请刷新后重新回答")
	}
	key := id + "\x00" + requestID
	b.mu.Lock()
	if b.asyncReplies == nil {
		b.asyncReplies = map[string]*asyncReply{}
	}
	if b.asyncReplies[key] != nil {
		b.mu.Unlock()
		result, err, _ := b.existingAsyncReply(id, requestID, response)
		return result, err
	}
	reply := &asyncReply{fingerprint: asyncResponseHash(response), done: make(chan struct{}), status: "pending"}
	b.asyncReplies[key] = reply
	if err := b.saveAsyncRepliesLocked(); err != nil {
		delete(b.asyncReplies, key)
		b.mu.Unlock()
		return nil, err
	}
	b.mu.Unlock()
	mode := "send"
	turns := model.OrderedTurns(state)
	if len(turns.Items) > 0 {
		if turn, ok := turns.Items[len(turns.Items)-1].(*jsoncompat.Object); ok && turn.MustGet("status") == "inProgress" {
			mode = "steer"
		}
	}
	// 确定性消息身份使传输与桌面回放可关联；不能生成一条新的随机身份重发。
	submissionID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:async-answer:"+key)).String()
	result, callErr := b.StartTurnWithOptions(id, submissionID, text, mode, "", nil, nil)
	if callErr != nil {
		// 已进入外部写路径，错误不证明未送达；保留占用并禁止用户盲目再发。
		callErr = errors.New("答复结果尚未确认，请在原会话核查；未自动重发：" + callErr.Error())
	}
	b.mu.Lock()
	reply.result, reply.err = result, callErr
	reply.status = "accepted"
	if callErr != nil {
		reply.status = "unknown"
	}
	if err := b.saveAsyncRepliesLocked(); err != nil {
		reply.result, reply.err, reply.status = nil, errors.New("答复已尝试提交，但提交记录保存失败；请在原会话核查，不要重发"), "unknown"
		result, callErr = nil, reply.err
	}
	close(reply.done)
	b.mu.Unlock()
	return result, callErr
}

func asyncQuestionReplyText(pending, response *jsoncompat.Object) (string, error) {
	params, _ := pending.MustGet("params").(*jsoncompat.Object)
	if params == nil || response == nil {
		return "", errors.New("请回答所有问题")
	}
	answers, _ := response.MustGet("answers").(*jsoncompat.Object)
	questions, _ := params.MustGet("questions").(*jsoncompat.Array)
	if answers == nil || questions == nil || len(questions.Items) == 0 || answers.Len() != len(questions.Items) {
		return "", errors.New("请完整回答当前问题，不要提交已过期的答案")
	}
	replies := &jsoncompat.Array{}
	for _, value := range questions.Items {
		question, _ := value.(*jsoncompat.Object)
		if question == nil {
			return "", errors.New("问题格式无效，请刷新聊天")
		}
		qid, _ := question.MustGet("id").(string)
		parts, _ := answers.MustGet(qid).(*jsoncompat.Array)
		if parts == nil || len(parts.Items) == 0 || len(parts.Items) > 10 {
			return "", errors.New("请回答所有问题")
		}
		texts := []string{}
		for _, part := range parts.Items {
			text, ok := part.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return "", errors.New("回复内容不能为空")
			}
			texts = append(texts, text)
		}
		answer := strings.Join(texts, "\n")
		if len([]rune(answer)) > 20000 {
			return "", errors.New("每个问题的回复不能超过 20000 字")
		}
		row := jsoncompat.NewObject()
		row.Set("questionItemId", qid)
		row.Set("question", question.MustGet("question"))
		row.Set("answer", answer)
		replies.Items = append(replies.Items, row)
	}
	return "<send_user_message_question_reply>\n" + string(jsoncompat.EncodeCompact(replies)) + "\n</send_user_message_question_reply>", nil
}
