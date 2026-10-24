package team

// Journal 是 team-journal.jsonl 的追加句柄（§8.1）：运行事实的来源，每条
// 记录一行、sequence 全局单调递增。打开时重放全量并修复崩溃残留——无换行
// 结尾但内容完整的尾行补回换行，解析不了的半行截断掉；重放途中遇到解析
// 失败、序号缺失、重复或回退直接报错，绝不静默丢弃事实。依赖顺序校验
// （task_completed 不得早于同 task 的 task_dispatching）由 recovery 侧的
// fold 负责，这里只保证序号本身严格递增。

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// journalTail 描述文件尾部的状态。
type journalTail int

const (
	tailComplete journalTail = iota // 以换行结束（或为空）
	tailAccepted                    // 尾行缺换行但内容完整有效
	tailTorn                        // 崩溃残留的半行
)

// Journal 持有 O_APPEND 句柄；Append 与 Replay 并发安全。
type Journal struct {
	mu           sync.Mutex
	path         string
	file         *os.File
	lastSequence uint64
}

// OpenJournal 重放既有 journal、修复尾部残留并打开追加句柄。文件不存在
// 视为空 journal。
func OpenJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("team journal 读取失败: %w", err)
	}
	events, tail, torn, err := scanJournal(raw)
	if err != nil {
		return nil, err
	}
	journal := &Journal{path: path}
	for _, event := range events {
		if event.Sequence > journal.lastSequence {
			journal.lastSequence = event.Sequence
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("team journal 打开失败: %w", err)
	}
	switch tail {
	case tailAccepted:
		// 只差一个换行符：补上，后续追加自然对齐行首。
		if _, err := file.Write([]byte{'\n'}); err != nil {
			file.Close()
			return nil, fmt.Errorf("team journal 修复失败: %w", err)
		}
	case tailTorn:
		// 截掉崩溃残留的半行，保证后续追加从干净的行首开始。
		if err := file.Truncate(int64(len(raw) - torn)); err != nil {
			file.Close()
			return nil, fmt.Errorf("team journal 修复失败: %w", err)
		}
		log.Printf("team journal 尾部有不完整记录，已截断 %d 字节: path=%s", torn, path)
	}
	journal.file = file
	return journal, nil
}

// Append 盖写单调 Sequence（调用方无需也不应自行编号），把整行一次性写入
// O_APPEND 句柄；写失败时 lastSequence 不前进，调用方应视为记录未落账。
func (j *Journal) Append(event *Event) error {
	if event == nil {
		return errors.New("event 不能为 nil")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return errors.New("team journal 已关闭")
	}
	event.Sequence = j.lastSequence + 1
	line := append(jsoncompat.EncodeCompact(eventToObject(event)), '\n')
	if _, err := j.file.Write(line); err != nil {
		return fmt.Errorf("team journal 写入失败: %w", err)
	}
	j.lastSequence = event.Sequence
	return nil
}

// Replay 从磁盘重读全部 journal 记录（含打开时接受的尾行），尾部半行再次
// 出现也只容忍不报错（打开时已修复，这里防御并发观察）。
func (j *Journal) Replay() ([]*Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	raw, err := os.ReadFile(j.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("team journal 读取失败: %w", err)
	}
	events, _, _, err := scanJournal(raw)
	if err != nil {
		return nil, err
	}
	return events, nil
}

// LastSequence 返回已落账的最大序号（空 journal 为 0）。
func (j *Journal) LastSequence() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastSequence
}

// Close 关闭追加句柄；之后的 Append 返回错误。
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

// scanJournal 逐行解析 journal 内容：完整行必须解析成功且序号严格递增
// （缺失为 0 同样非法）；无换行结尾的尾段若能解析为合法事件则接受
// （torn 为 0、tail=tailAccepted），否则视为崩溃残留（torn 记录残留字节数）。
func scanJournal(raw []byte) (events []*Event, tail journalTail, torn int, err error) {
	if len(raw) == 0 {
		return nil, tailComplete, 0, nil
	}
	previous := uint64(0)
	offset := 0
	for offset < len(raw) {
		newline := bytes.IndexByte(raw[offset:], '\n')
		if newline < 0 {
			segment := raw[offset:]
			trimmed := bytes.TrimSpace(segment)
			if len(trimmed) == 0 {
				return events, tailTorn, len(segment), nil
			}
			event, parseErr := parseEventLine(trimmed)
			if parseErr != nil {
				return events, tailTorn, len(segment), nil
			}
			if err := checkSequence(previous, event); err != nil {
				return nil, tailComplete, 0, err
			}
			return append(events, event), tailAccepted, 0, nil
		}
		line := bytes.TrimSpace(raw[offset : offset+newline])
		offset += newline + 1
		if len(line) == 0 {
			continue
		}
		event, parseErr := parseEventLine(line)
		if parseErr != nil {
			return nil, tailComplete, 0, fmt.Errorf("team journal 第 %d 条记录解析失败: %w", len(events)+1, parseErr)
		}
		if err := checkSequence(previous, event); err != nil {
			return nil, tailComplete, 0, err
		}
		previous = event.Sequence
		events = append(events, event)
	}
	return events, tailComplete, 0, nil
}

func parseEventLine(line []byte) (*Event, error) {
	event := &Event{}
	if err := event.UnmarshalJSON(line); err != nil {
		return nil, err
	}
	return event, nil
}

// checkSequence 保证序号从 1 起、严格递增：重复或回退都说明 journal 损坏
// 或被并发写坏，必须报错而不是静默继续。
func checkSequence(previous uint64, event *Event) error {
	if event.Sequence == 0 {
		return fmt.Errorf("team journal 记录缺少 sequence: id=%s type=%s", event.ID, event.Type)
	}
	if event.Sequence <= previous {
		return fmt.Errorf("team journal 序号重复或回退: 上一条=%d 当前=%d id=%s", previous, event.Sequence, event.ID)
	}
	return nil
}
