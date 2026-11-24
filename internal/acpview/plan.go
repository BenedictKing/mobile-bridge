package acpview

import (
	"crypto/sha256"
	"fmt"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type planRevision struct {
	Number   int
	Hash     string
	Content  string
	Previous string
}

type planRevisions map[string]planRevision

func (revisions planRevisions) track(id, content string) planRevision {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	current, exists := revisions[id]
	if exists && current.Hash == hash {
		return current
	}
	next := planRevision{Number: 1, Hash: hash, Content: content}
	if exists {
		next.Number = current.Number + 1
		next.Previous = current.Content
	}
	revisions[id] = next
	return next
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
