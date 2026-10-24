package team

// EventFeed 是单个 Run 的事件投影（§10.3）：环形缓冲最近 500 条 journal
// 事件，Changes 的降级契约照抄 internal/timeline 的 Timeline.Changes——
// epoch 不匹配或游标已滑出环形缓冲时返回 reset 整页（最近 ≤20 条），否则
// 返回 after 之后的事件行。不复用 timeline.Timeline：那是 message-row
// 模型，事件流只需要 epoch/sequence + 环形缓冲。meta 由 httpd 注入 Run
// 状态摘要。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	// feedRingCapacity 是每 Run 保留在内存的最近事件数。
	feedRingCapacity = 500
	// feedResetRows 是 reset 整页携带的最近事件数。
	feedResetRows = 20
)

// EventFeed 的 sequence 直接取 journal 序号（跨 Run 全局单调，单 Run 内
// 有空洞）；epoch 在每次创建 feed（网关加载）时随机生成，旧游标全部失效。
type EventFeed struct {
	mu       sync.Mutex
	runID    string
	epoch    string
	ring     []*Event
	first    uint64 // 进入过本 feed 的第一个事件序号（0=还没有事件）
	sequence uint64 // 最新事件序号（0=还没有事件）
	notify   chan struct{}
	meta     *jsoncompat.Object
}

// NewEventFeed 创建指定 Run 的事件投影，seed 通常是网关重启后从 journal
// 重放出的该 Run 事件（按序号升序）。runID 非空时，属于其他 Run 的事件
// 会被 Add 拒绝。
func NewEventFeed(runID string, seed []*Event) *EventFeed {
	feed := &EventFeed{
		runID:  runID,
		epoch:  randomEpoch(),
		notify: make(chan struct{}, 1),
		meta:   jsoncompat.NewObject(),
	}
	for _, event := range seed {
		feed.Add(event)
	}
	return feed
}

// Add 把一条 journal 事件并入投影：序号必须大于当前最新（journal 单调
// 追加，重放/乱序的旧事件忽略），返回是否真的并入。并入后向 Notify 通道
// 非阻塞地发一个信号（容量 1，慢消费者只丢信号不丢数据——下次 Changes
// 仍能拿到全部增量）。
func (f *EventFeed) Add(event *Event) bool {
	if event == nil {
		return false
	}
	f.mu.Lock()
	if f.runID != "" && event.RunID != f.runID {
		f.mu.Unlock()
		return false
	}
	if event.Sequence <= f.sequence {
		// journal 序号从 1 起：0（未落账）与重复/乱序的旧事件一并忽略。
		f.mu.Unlock()
		return false
	}
	if len(f.ring) >= feedRingCapacity {
		// 前移复用底层数组，避免环形切片无限增长。
		copy(f.ring, f.ring[1:])
		f.ring[len(f.ring)-1] = event
	} else {
		f.ring = append(f.ring, event)
	}
	if f.first == 0 {
		f.first = event.Sequence
	}
	f.sequence = event.Sequence
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
	return true
}

// Changes 返回 after 之后的事件行。epoch 不匹配、after 不在环形缓冲内
// （或 after==0 而最早事件已被挤出）时降级为 reset 整页。返回结构：
// {epoch, sequence, rows, meta} 或 {epoch, sequence, rows, meta, reset:true}。
func (f *EventFeed) Changes(after uint64, epoch string) *jsoncompat.Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	if epoch != f.epoch {
		return f.resetPageLocked()
	}
	start := -1
	if after == 0 {
		if f.first == 0 || len(f.ring) == 0 || f.ring[0].Sequence != f.first {
			return f.resetPageLocked()
		}
		start = 0
	} else {
		for index, event := range f.ring {
			if event.Sequence == after {
				start = index + 1
				break
			}
		}
		if start < 0 {
			if after == f.sequence {
				start = len(f.ring) // 已是最新，增量必然为空
			} else {
				return f.resetPageLocked() // 游标滑出缓冲
			}
		}
	}
	rows := &jsoncompat.Array{}
	for _, event := range f.ring[start:] {
		rows.Items = append(rows.Items, eventToObject(event))
	}
	result := jsoncompat.NewObject()
	result.Set("epoch", f.epoch)
	result.Set("sequence", json.Number(strconv.FormatUint(f.sequence, 10)))
	result.Set("rows", rows)
	result.Set("meta", jsoncompat.Clone(f.meta))
	return result
}

func (f *EventFeed) resetPageLocked() *jsoncompat.Object {
	rows := &jsoncompat.Array{}
	start := len(f.ring) - feedResetRows
	if start < 0 {
		start = 0
	}
	for _, event := range f.ring[start:] {
		rows.Items = append(rows.Items, eventToObject(event))
	}
	result := jsoncompat.NewObject()
	result.Set("epoch", f.epoch)
	result.Set("sequence", json.Number(strconv.FormatUint(f.sequence, 10)))
	result.Set("rows", rows)
	result.Set("meta", jsoncompat.Clone(f.meta))
	result.Set("reset", true)
	return result
}

// Notify 返回变更信号通道（容量 1）：每次成功 Add 后非阻塞投递，供 SSE /
// 长轮询等待使用；读不到信号不代表无变更，调用方仍应周期性读 Changes。
func (f *EventFeed) Notify() <-chan struct{} { return f.notify }

// SetMeta 更新随 Changes 返回的状态摘要（httpd 注入 Run 状态）。
func (f *EventFeed) SetMeta(meta *jsoncompat.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if meta == nil {
		f.meta = jsoncompat.NewObject()
		return
	}
	f.meta = jsoncompat.Clone(meta).(*jsoncompat.Object)
}

// Epoch 返回当前 epoch。
func (f *EventFeed) Epoch() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch
}

// Sequence 返回最新事件序号（0=还没有事件）。
func (f *EventFeed) Sequence() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sequence
}

// Cursor 返回 "epoch.sequence" 形式的游标，SSE 帧 id 与客户端回传共用。
func (f *EventFeed) Cursor() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch + "." + strconv.FormatUint(f.sequence, 10)
}

// Events 返回环形缓冲内的事件副本（旧→新），供 Run 详情与调试使用。
func (f *EventFeed) Events() []*Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	events := make([]*Event, len(f.ring))
	copy(events, f.ring)
	return events
}

// randomEpoch 与 timeline 同款：16 字节 crypto/rand 的 hex。
func randomEpoch() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}
