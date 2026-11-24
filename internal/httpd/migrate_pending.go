package httpd

import (
	"errors"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// migrationPendingView adds user compositions without changing native history
// or the source ledger. Queue metadata also filters internal task/goal sends.
func (s *Server) migrationPendingView(threadID string, view *jsoncompat.Object, uiLocale string) (*jsoncompat.Object, error) {
	if s.Queue == nil {
		return view, nil
	}
	texts := migrationTextsByLocale[normalizeUILocale(uiLocale)]
	pending := &jsoncompat.Array{}
	for _, raw := range s.queueMetaExcludingActivations(threadID).Items {
		row, ok := raw.(*jsoncompat.Object)
		if !ok {
			continue
		}
		switch stringField(row, "status") {
		case SubUnknown:
			return nil, errors.New("有用户消息发送结果待确认，请先核实原会话后再迁移或压缩")
		case SubQueued, SubFailed:
		default:
			continue
		}
		text := texts.pending + "\n" + strings.TrimSpace(stringField(row, "text"))
		ids := map[string]bool{}
		if attachments, ok := row.MustGet("attachmentIds").(*jsoncompat.Array); ok {
			for _, raw := range attachments.Items {
				if id, ok := raw.(string); ok {
					ids[id] = true
				}
			}
		}
		names := []string{}
		if attachments, ok := row.MustGet("attachments").(*jsoncompat.Array); ok {
			for _, raw := range attachments.Items {
				if name, ok := raw.(string); ok && !ids[name] {
					names = append(names, name)
				}
			}
		}
		if len(ids) > 0 || len(names) > 0 {
			text += "\n" + texts.attachmentsOmitted + strings.Join(names, ", ")
		}
		pending.Items = append(pending.Items, text)
	}
	result := jsoncompat.Clone(view).(*jsoncompat.Object)
	result.Set("migrationPendingMessages", pending)
	return result, nil
}
