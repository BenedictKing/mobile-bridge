package httpd

import (
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/planstore"
)

// planHistoryJSON 投影只读历史：每个版本附带它收到的修订反馈。
func planHistoryJSON(kind, threadID, planID string, plan planstore.Plan, found bool) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("agent", kind)
	result.Set("sessionId", threadID)
	result.Set("planId", planID)
	revisions := &jsoncompat.Array{}
	if found {
		for _, revision := range plan.Revisions {
			entry := jsoncompat.NewObject()
			entry.Set("version", jsoncompat.Num(revision.Version))
			entry.Set("hash", revision.Hash)
			entry.Set("content", revision.Content)
			entry.Set("createdAt", revision.CreatedAt.Format(time.RFC3339Nano))
			feedbackItems := &jsoncompat.Array{}
			for _, feedback := range revision.Feedback {
				item := jsoncompat.NewObject()
				item.Set("text", feedback.Text)
				item.Set("createdAt", feedback.CreatedAt.Format(time.RFC3339Nano))
				annotations := &jsoncompat.Array{}
				for _, annotation := range feedback.Annotations {
					value := jsoncompat.NewObject()
					value.Set("id", annotation.ID)
					value.Set("type", annotation.Type)
					value.Set("quote", annotation.Quote)
					value.Set("body", annotation.Body)
					annotations.Items = append(annotations.Items, value)
				}
				item.Set("annotations", annotations)
				feedbackItems.Items = append(feedbackItems.Items, item)
			}
			entry.Set("feedback", feedbackItems)
			revisions.Items = append(revisions.Items, entry)
		}
	}
	result.Set("revisions", revisions)
	return result
}

var errInvalidPlanFeedback = errors.New("计划批注格式不正确")

// parsePlanFeedback 解析 ACP 回应附带的 planFeedback；缺省返回 nil。非法格式
// 在送达 Agent 之前拒绝，避免意见只到达一半却记成另一份历史。
func parsePlanFeedback(object *jsoncompat.Object, requestID string) (*planstore.Feedback, error) {
	value, exists := object.Get("planFeedback")
	if !exists || value == nil {
		return nil, nil
	}
	raw, ok := value.(*jsoncompat.Object)
	if !ok {
		return nil, errInvalidPlanFeedback
	}
	feedback := &planstore.Feedback{RequestID: requestID}
	for _, key := range raw.Keys() {
		switch key {
		case "text":
			text, ok := raw.MustGet(key).(string)
			if !ok {
				return nil, errInvalidPlanFeedback
			}
			feedback.Text = text
		case "annotations":
			list, ok := raw.MustGet(key).(*jsoncompat.Array)
			if !ok || len(list.Items) > planstore.MaxAnnotations {
				return nil, errInvalidPlanFeedback
			}
			for _, item := range list.Items {
				annotation, ok := parseAnnotation(item)
				if !ok {
					return nil, errInvalidPlanFeedback
				}
				feedback.Annotations = append(feedback.Annotations, annotation)
			}
		default:
			return nil, errInvalidPlanFeedback
		}
	}
	if planstore.ValidateFeedback(*feedback) != nil {
		return nil, errInvalidPlanFeedback
	}
	return feedback, nil
}

func parseAnnotation(value any) (planstore.Annotation, bool) {
	raw, ok := value.(*jsoncompat.Object)
	if !ok {
		return planstore.Annotation{}, false
	}
	var annotation planstore.Annotation
	for _, key := range raw.Keys() {
		text, isString := raw.MustGet(key).(string)
		if !isString {
			return planstore.Annotation{}, false
		}
		switch key {
		case "id":
			annotation.ID = text
		case "type":
			annotation.Type = text
		case "quote":
			annotation.Quote = text
		case "body":
			annotation.Body = text
		default:
			return planstore.Annotation{}, false
		}
	}
	return annotation, true
}

// planRequestRef 从服务端 pending 请求投影读取计划身份；不是计划请求时 ok=false。
func planRequestRef(params *jsoncompat.Object) (planID string, version int, hash string, ok bool) {
	if params == nil {
		return "", 0, "", false
	}
	planID, _ = params.MustGet("planId").(string)
	hash, _ = params.MustGet("planHash").(string)
	number, _ := params.MustGet("planVersion").(json.Number)
	if planID == "" || hash == "" {
		return "", 0, "", false
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 1 {
		return "", 0, "", false
	}
	return planID, int(parsed), hash, true
}

// recordPlanFeedback 在回应成功后追加历史；失败只记日志，不改变已送达的回应。
func (h *handler) recordPlanFeedback(kind, threadID string, params *jsoncompat.Object, feedback *planstore.Feedback) {
	if feedback == nil || h.server.Plans == nil {
		return
	}
	planID, version, hash, ok := planRequestRef(params)
	if !ok {
		return
	}
	if err := h.server.Plans.RecordFeedback(kind, threadID, planID, version, hash, *feedback); err != nil {
		log.Printf("计划反馈保存失败: agent=%s session=%s plan=%s version=%d: %v", kind, threadID, planID, version, err)
	}
}
