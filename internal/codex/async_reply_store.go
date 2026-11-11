package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type asyncReplyReceipt struct {
	Fingerprint string          `json:"fingerprint"`
	Status      string          `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// 外部发送前先保存 pending；进程若在确认前退出，重启后按 unknown 处理。
// 只保存答案哈希、请求身份和收据，不保存原始答案正文。
func (b *Bridge) loadAsyncRepliesLocked() error {
	if b.asyncRepliesLoaded {
		return nil
	}
	if b.asyncReplies == nil {
		b.asyncReplies = map[string]*asyncReply{}
	}
	if b.AsyncReplyPath == "" {
		b.asyncRepliesLoaded = true
		return nil
	}
	raw, err := os.ReadFile(b.AsyncReplyPath)
	if errors.Is(err, os.ErrNotExist) {
		b.asyncRepliesLoaded = true
		return nil
	}
	if err != nil {
		return errors.New("读取答复提交记录失败，请检查网关数据目录")
	}
	var receipts map[string]asyncReplyReceipt
	if json.Unmarshal(raw, &receipts) != nil {
		return errors.New("答复提交记录损坏，请在原会话核查；未自动重发")
	}
	for key, receipt := range receipts {
		if receipt.Fingerprint == "" {
			return errors.New("答复提交记录无效，请在原会话核查；未自动重发")
		}
		reply := &asyncReply{fingerprint: receipt.Fingerprint, done: make(chan struct{}), status: receipt.Status}
		if receipt.Status == "accepted" {
			if len(receipt.Result) > 0 {
				value, err := jsoncompat.Unmarshal(receipt.Result)
				if err == nil {
					reply.result, _ = value.(*jsoncompat.Object)
				}
			}
			if reply.result == nil {
				reply.result = jsoncompat.NewObject()
				reply.result.Set("ok", true)
			}
		} else {
			reply.status = "unknown"
			reply.err = errors.New("答复结果尚未确认，请在原会话核查；未自动重发")
		}
		close(reply.done)
		b.asyncReplies[key] = reply
	}
	b.asyncRepliesLoaded = true
	return nil
}

func (b *Bridge) saveAsyncRepliesLocked() error {
	if b.AsyncReplyPath == "" {
		return nil
	}
	receipts := map[string]asyncReplyReceipt{}
	for key, reply := range b.asyncReplies {
		receipt := asyncReplyReceipt{Fingerprint: reply.fingerprint, Status: reply.status}
		if reply.result != nil {
			receipt.Result = jsoncompat.EncodeCompact(reply.result)
		}
		receipts[key] = receipt
	}
	raw, err := json.Marshal(receipts)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(b.AsyncReplyPath), ".question-replies-*")
	if err != nil {
		return errors.New("保存答复提交记录失败，尚未发送答案")
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary, b.AsyncReplyPath)
	}
	if err != nil {
		return errors.New("保存答复提交记录失败，尚未发送答案")
	}
	return nil
}
