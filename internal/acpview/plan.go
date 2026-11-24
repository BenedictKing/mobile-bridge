package acpview

import (
	"log"

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

func requestPlanID(toolCall, planInput *jsoncompat.Object, requestID string) string {
	if id, _ := planInput.MustGet("planId").(string); id != "" {
		return id
	}
	if id, _ := toolCall.MustGet("toolCallId").(string); id != "" {
		return id
	}
	return requestID
}
