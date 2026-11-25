package httpd

import (
	"net/http"
	"sort"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// handleInbox 汇总各 Agent 已加载会话的待处理请求，供跨 Agent 待处理列表使用。
// 只读：不打开未加载会话、不接管 Codex 桌面会话，也不接受回应——回应仍走原聊天
// 的 pending request，审批协议与命令账本不变。
func (h *handler) handleInbox() {
	result := jsoncompat.NewObject()
	result.Set("items", h.server.inboxItems())
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// inboxItems 是 Inbox 列表与侧栏徽标的唯一数据来源，两者计数始终一致。
func (s *Server) inboxItems() *jsoncompat.Array {
	entries := []*inboxEntry{}
	for kind, manager := range s.ACPAgents {
		if manager == nil {
			continue
		}
		for _, session := range manager.Active() {
			pending := session.PendingRequests()
			for _, request := range pending.Requests {
				if item := newInboxItem(kind, "local", session.ID, pending.Title, request); item != nil {
					entries = append(entries, &inboxEntry{object: item, recency: pending.Recency,
						agent: kind, title: pending.Title, requestID: stringField(item, "requestId")})
				}
			}
		}
	}
	if s.Codex != nil {
		for _, row := range s.Codex.PendingRequests() {
			id, _ := row.MustGet("id").(string)
			host, _ := row.MustGet("host").(string)
			title, _ := row.MustGet("title").(string)
			requests, _ := row.MustGet("requests").(*jsoncompat.Array)
			if requests == nil {
				continue
			}
			for _, value := range requests.Items {
				request, _ := value.(*jsoncompat.Object)
				if request == nil {
					continue
				}
				if item := newInboxItem("codex", host, id, title, request); item != nil {
					entries = append(entries, &inboxEntry{object: item, agent: "codex",
						title: title, requestID: stringField(item, "requestId")})
				}
			}
		}
	}
	// 最近活动的会话优先；同会话内保持请求原有顺序，避免列表跳动。
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.recency != b.recency {
			return a.recency > b.recency
		}
		if a.agent != b.agent {
			return a.agent < b.agent
		}
		if a.title != b.title {
			return a.title < b.title
		}
		return a.requestID < b.requestID
	})
	items := &jsoncompat.Array{}
	for _, entry := range entries {
		items.Items = append(items.Items, entry.object)
	}
	return items
}

type inboxEntry struct {
	object    *jsoncompat.Object
	recency   int64
	agent     string
	title     string
	requestID string
}

// newInboxItem 把一张待处理卡片裁剪成列表行：不带计划正文、命令详情等大字段，
// 只保留跳转所需的会话身份和展示所需的摘要。
func newInboxItem(agent, host, sessionID, title string, request *jsoncompat.Object) *jsoncompat.Object {
	method, _ := request.MustGet("method").(string)
	requestID, _ := request.MustGet("id").(string)
	if method == "" && requestID == "" {
		return nil
	}
	params, _ := request.MustGet("params").(*jsoncompat.Object)
	kind, summary := inboxSummary(method, params)
	item := jsoncompat.NewObject()
	item.Set("agent", agent)
	item.Set("sessionId", sessionID)
	item.Set("host", host)
	item.Set("title", title)
	item.Set("requestId", requestID)
	item.Set("method", method)
	item.Set("kind", kind)
	item.Set("summary", summary)
	item.Set("supported", request.MustGet("supported") == true)
	if params != nil {
		if planID, _ := params.MustGet("planId").(string); planID != "" {
			item.Set("planId", planID)
		}
		if version, ok := params.Get("planVersion"); ok {
			item.Set("planVersion", jsoncompat.Clone(version))
		}
	}
	if recommend, ok := request.Get("recommend"); ok {
		item.Set("recommend", jsoncompat.Clone(recommend))
	}
	return item
}

// inboxSummary 把不同 Agent 的请求方言归为四类：计划审阅、权限确认、Agent 提问、
// 结构化输入。摘要只取标题/首行，正文留在原聊天里。
func inboxSummary(method string, params *jsoncompat.Object) (string, string) {
	if params == nil {
		return "permission", method
	}
	text := func(key string) string {
		value, _ := params.MustGet(key).(string)
		return value
	}
	switch {
	case method == "acp/requestInput":
		return "input", clipInboxText(text("message"))
	case method == "acp/requestPermission" && params.MustGet("question") == true:
		return "question", clipInboxText(text("text"))
	case method == "acp/requestPermission" && strings.TrimSpace(text("planContent")) != "":
		return "plan", clipInboxText(text("planContent"))
	case method == "item/plan/requestImplementation":
		return "plan", clipInboxText(text("text"))
	case strings.Contains(method, "requestUserInput"):
		return "input", clipInboxText(text("message"))
	}
	summary := strings.TrimSpace(strings.Join([]string{text("title"), text("command")}, " "))
	if summary == "" {
		summary = text("text")
	}
	if summary == "" {
		summary = method
	}
	return "permission", clipInboxText(summary)
}

// clipInboxText 压缩空白并限制摘要长度，避免一张大计划撑爆列表。
func clipInboxText(value string) string {
	fields := strings.Join(strings.Fields(value), " ")
	if fields == "" {
		return ""
	}
	runes := []rune(fields)
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return fields
}
