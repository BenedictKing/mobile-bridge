package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/accpool"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func teamPoolKindSupported(pool *accpool.Manager, kind string) bool {
	if pool == nil {
		return false
	}
	for _, candidate := range pool.Kinds() {
		if candidate == kind {
			return true
		}
	}
	return false
}

func teamAgentKinds(teamRow *team.Team) []string {
	if teamRow == nil {
		return nil
	}
	kinds := []string{}
	seen := map[string]bool{}
	for _, member := range teamRow.Members {
		if !member.Enabled || member.AgentKind == "" || seen[member.AgentKind] {
			continue
		}
		seen[member.AgentKind] = true
		kinds = append(kinds, member.AgentKind)
	}
	return kinds
}

// teamAgentQuotas projects only current-account snapshots for enabled member
// kinds. ActiveQuota performs local reads only; no quota refresh is triggered.
func teamAgentQuotas(server *Server, teamRow *team.Team) *jsoncompat.Array {
	rows := &jsoncompat.Array{}
	for _, agentKind := range teamAgentKinds(teamRow) {
		row := jsoncompat.NewObject()
		row.Set("agent", agentKind)
		row.Set("accountStatus", nil)
		row.Set("quota", nil)
		row.Set("quotaUpdatedAt", nil)
		row.Set("quotaError", nil)
		row.Set("windows", &jsoncompat.Array{})
		poolKind := accpool.PoolKind(agentKind)
		if server == nil || server.Pool == nil {
			row.Set("status", "unavailable")
			rows.Items = append(rows.Items, row)
			continue
		}
		if !teamPoolKindSupported(server.Pool, poolKind) {
			row.Set("status", "not_supported")
			rows.Items = append(rows.Items, row)
			continue
		}
		snapshot, err := server.Pool.ActiveQuota(poolKind)
		if err != nil {
			row.Set("status", "unavailable")
			rows.Items = append(rows.Items, row)
			continue
		}
		for _, key := range snapshot.Keys() {
			if key == "kind" {
				continue
			}
			value, _ := snapshot.Get(key)
			row.Set(key, value)
		}
		if quota, _ := snapshot.MustGet("quota").(*jsoncompat.Object); quota != nil {
			row.Set("windows", teamQuotaWindows(quota))
		} else {
			row.Set("windows", &jsoncompat.Array{})
		}
		rows.Items = append(rows.Items, row)
	}
	return rows
}

func teamQuotaNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	case float64:
		return number, true
	case int64:
		return float64(number), true
	case int:
		return float64(number), true
	default:
		return 0, false
	}
}

func teamQuotaJSONNumber(value float64) json.Number {
	return json.Number(strconv.FormatFloat(value, 'f', -1, 64))
}

// teamQuotaWindows normalizes every model-relevant window with explicit
// remaining amounts. Auxiliary quotas (for example MCP-only usage) are omitted.
func teamQuotaWindows(quota *jsoncompat.Object) *jsoncompat.Array {
	rows := &jsoncompat.Array{}
	if quota == nil {
		return rows
	}
	windows := []*jsoncompat.Object{}
	if weekly, _ := quota.MustGet("weekly").(*jsoncompat.Object); weekly != nil {
		windows = append(windows, weekly)
	}
	if limits, _ := quota.MustGet("limits").(*jsoncompat.Array); limits != nil {
		for _, value := range limits.Items {
			if window, _ := value.(*jsoncompat.Object); window != nil {
				windows = append(windows, window)
			}
		}
	}
	for _, window := range windows {
		if auxiliary, _ := window.MustGet("auxiliary").(bool); auxiliary {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("name", window.MustGet("name"))
		row.Set("windowUnit", window.MustGet("windowUnit"))
		row.Set("windowDuration", window.MustGet("windowDuration"))
		row.Set("used", window.MustGet("used"))
		row.Set("limit", window.MustGet("limit"))
		row.Set("resetAt", window.MustGet("resetAt"))
		used, usedOK := teamQuotaNumber(window.MustGet("used"))
		limit, limitOK := teamQuotaNumber(window.MustGet("limit"))
		if usedOK && limitOK {
			remaining := limit - used
			if remaining < 0 || limit <= 0 {
				remaining = 0
			} else if remaining > limit {
				remaining = limit
			}
			row.Set("remaining", teamQuotaJSONNumber(remaining))
			if limit > 0 {
				row.Set("remainingPercent", teamQuotaJSONNumber(remaining/limit*100))
			} else {
				row.Set("remainingPercent", nil)
			}
		} else {
			row.Set("remaining", nil)
			row.Set("remainingPercent", nil)
		}
		rows.Items = append(rows.Items, row)
	}
	return rows
}

// handleTeamQuotas serves GET /api/teams/:id/quotas. The response contains no
// account identifiers or credentials and never performs a network refresh.
func (h *handler) handleTeamQuotas(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	result := jsoncompat.NewObject()
	result.Set("agents", teamAgentQuotas(h.server, teamRow))
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func teamQuotaText(value any, fallback string) string {
	text := strings.Join(strings.Fields(fmt.Sprint(value)), " ")
	if text == "" || text == "<nil>" {
		return fallback
	}
	return truncateSubText(text, 80)
}

func teamQuotaNumberText(value any) string {
	if number, ok := teamQuotaNumber(value); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return "unknown"
}

func teamQuotaTimeText(value any) string {
	milliseconds, ok := teamQuotaNumber(value)
	if !ok || milliseconds <= 0 {
		return "unknown"
	}
	return time.UnixMilli(int64(milliseconds)).UTC().Format(time.RFC3339)
}

// NewTeamLeadBriefingSource supplies the same sanitized quota projection to
// each Lead dispatch. The snapshot is planning context, not a freshness promise.
func NewTeamLeadBriefingSource(server *Server) func(*team.Team) string {
	return func(teamRow *team.Team) string {
		rows := teamAgentQuotas(server, teamRow)
		if len(rows.Items) == 0 {
			return ""
		}
		var builder strings.Builder
		builder.WriteString("[Team account quotas]\n")
		for _, value := range rows.Items {
			row, _ := value.(*jsoncompat.Object)
			if row == nil {
				continue
			}
			fmt.Fprintf(&builder, "- agent=%s · status=%s", row.MustGet("agent"), row.MustGet("status"))
			if status := teamQuotaText(row.MustGet("accountStatus"), ""); status != "" {
				builder.WriteString(" · accountStatus=" + status)
			}
			if row.MustGet("quotaUpdatedAt") != nil {
				builder.WriteString(" · quotaUpdatedAt=" + teamQuotaTimeText(row.MustGet("quotaUpdatedAt")))
			}
			builder.WriteString("\n")
			windows, _ := row.MustGet("windows").(*jsoncompat.Array)
			if windows == nil {
				windows = &jsoncompat.Array{}
			}
			for index, windowValue := range windows.Items {
				if index >= 12 {
					builder.WriteString("  - additional windows omitted\n")
					break
				}
				window, _ := windowValue.(*jsoncompat.Object)
				if window == nil {
					continue
				}
				fmt.Fprintf(&builder, "  - %s: remaining=%s/%s (%s%%)",
					teamQuotaText(window.MustGet("name"), "额度窗口"),
					teamQuotaNumberText(window.MustGet("remaining")),
					teamQuotaNumberText(window.MustGet("limit")),
					teamQuotaNumberText(window.MustGet("remainingPercent")))
				if resetAt := teamQuotaText(window.MustGet("resetAt"), ""); resetAt != "" {
					builder.WriteString(" · resetAt=" + resetAt)
				}
				builder.WriteString("\n")
			}
			quota, _ := row.MustGet("quota").(*jsoncompat.Object)
			if quota == nil && row.MustGet("status") == "ready" {
				builder.WriteString("  - quota=unknown\n")
			}
			if teamQuotaText(row.MustGet("quotaError"), "") != "" {
				builder.WriteString("  - quotaRefreshError=true\n")
			}
			if quota == nil {
				continue
			}
			if credits, _ := quota.MustGet("credits").(*jsoncompat.Object); credits != nil {
				fmt.Fprintf(&builder, "  - creditsRemaining=%s/%s\n",
					teamQuotaNumberText(credits.MustGet("totalRemaining")),
					teamQuotaNumberText(credits.MustGet("totalCapacity")))
			}
			if booster, _ := quota.MustGet("booster").(*jsoncompat.Object); booster != nil && booster.MustGet("balanceCents") != nil {
				fmt.Fprintf(&builder, "  - boosterBalanceCents=%s %s\n",
					teamQuotaNumberText(booster.MustGet("balanceCents")), teamQuotaText(booster.MustGet("currency"), ""))
			}
			if membership, _ := quota.MustGet("membership").(*jsoncompat.Object); membership != nil && membership.MustGet("creditBalance") != nil {
				builder.WriteString("  - membershipCreditBalance=" + teamQuotaNumberText(membership.MustGet("creditBalance")) + "\n")
			}
		}
		builder.WriteString("以上是网关本地账号额度快照，未做实时刷新，额度可能已经变化。制定工作流时结合成员 tags 与剩余额度安排任务；额度较低时优先选择额度充足且能力匹配的成员，不要为刷新额度执行命令，也不要复述账号隐私信息。\n")
		return truncateSubText(builder.String(), 12000)
	}
}
