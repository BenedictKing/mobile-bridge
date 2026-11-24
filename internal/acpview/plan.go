package acpview

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/planstore"
)

type planRevision struct {
	Number      int
	Hash        string
	Content     string
	Previous    string
	HasPrevious bool
}

type planRevisions map[string]planRevision

func (revisions planRevisions) track(id, content string) planRevision {
	hash := planstore.Hash(content)
	current, exists := revisions[id]
	if exists && current.Hash == hash {
		return current
	}
	next := planRevision{Number: 1, Hash: hash, Content: content}
	if exists {
		next.Number = current.Number + 1
		next.Previous = current.Content
		next.HasPrevious = true
	}
	revisions[id] = next
	return next
}

func (v *View) UsePlanStore(store *planstore.Store, agent string) {
	v.planStore = store
	v.planAgent = agent
}

func (v *View) trackPlan(id, content string) planRevision {
	if _, exists := v.planRevisions[id]; !exists && v.planStore != nil {
		if persisted, ok := v.planStore.Get(v.planAgent, v.SessionID, id); ok && len(persisted.Revisions) > 0 {
			latest := persisted.Revisions[len(persisted.Revisions)-1]
			current := planRevision{Number: latest.Version, Hash: latest.Hash, Content: latest.Content}
			if len(persisted.Revisions) > 1 {
				current.Previous = persisted.Revisions[len(persisted.Revisions)-2].Content
				current.HasPrevious = true
			}
			v.planRevisions[id] = current
		}
	}
	revision := v.planRevisions.track(id, content)
	if v.planStore != nil {
		if err := v.planStore.Record(v.planAgent, v.SessionID, id, revision.Number, revision.Hash, content); err != nil {
			log.Printf("计划历史保存失败: agent=%s session=%s plan=%s: %v", v.planAgent, v.SessionID, id, err)
		}
	}
	return revision
}

const kimiPlanPathPrefix = "Plan saved to: "

// kimiPlanInput 识别 Kimi acp-server 的计划审阅请求：它不带 rawInput.plan，
// 而是把 "Plan saved to: <path>\n\n<plan>" 作为首个内容块，末块固定为
// "Requesting approval to …"，选项为 plan_approve/plan_revise/…。
// 返回与 rawInput 同形的 {plan, planId}；planId 取计划文件名，跨修订稳定。
func kimiPlanInput(params, toolCall *jsoncompat.Object) *jsoncompat.Object {
	if params == nil || toolCall == nil {
		return nil
	}
	if title, _ := toolCall.MustGet("title").(string); title != "ExitPlanMode" || !hasOption(params, "plan_revise") {
		return nil
	}
	blocks := arr(toolCall.MustGet("content"))
	if blocks == nil || len(blocks.Items) < 2 {
		return nil
	}
	plan := FlattenOutput(&jsoncompat.Array{Items: blocks.Items[:1]})
	path := ""
	if rest, ok := strings.CutPrefix(plan, kimiPlanPathPrefix); ok {
		path, plan, _ = strings.Cut(rest, "\n\n")
	}
	if strings.TrimSpace(plan) == "" {
		return nil
	}
	input := jsoncompat.NewObject()
	input.Set("plan", plan)
	if id := strings.TrimSuffix(filepath.Base(strings.TrimSpace(path)), ".md"); path != "" && id != "" && id != "." {
		input.Set("planId", id)
	}
	return input
}

// KimiPlanPermission 报告该权限请求是否为 Kimi 内容块形态的计划审阅。
// Kimi 对这种请求不读取回应里的修改意见，需要网关代为转交。
func KimiPlanPermission(params *jsoncompat.Object) bool {
	if params == nil {
		return false
	}
	return kimiPlanInput(params, obj(params.MustGet("toolCall"))) != nil
}

func hasOption(params *jsoncompat.Object, optionID string) bool {
	if list := arr(params.MustGet("options")); list != nil {
		for _, item := range list.Items {
			if option := obj(item); option != nil && option.MustGet("optionId") == optionID {
				return true
			}
		}
	}
	return false
}

func requestPlanID(toolCall, planInput *jsoncompat.Object, requestID string) string {
	if id, _ := planInput.MustGet("planId").(string); id != "" {
		return id
	}
	if id, _ := toolCall.MustGet("toolCallId").(string); id != "" {
		return id
	}
	return requestID
}
