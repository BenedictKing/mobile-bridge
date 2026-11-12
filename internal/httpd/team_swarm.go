package httpd

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// 事件流只发送刷新信号；完整契约和证据走受认证详情，私有恢复账本不出站。
func teamEventForFeed(event *team.Event) *team.Event {
	if event == nil {
		return nil
	}
	copy := *event
	copy.Payload = map[string]any{}
	for k, v := range event.Payload {
		if k != "swarmTask" && k != "swarmState" {
			copy.Payload[k] = v
		}
	}
	if event.Type == team.EventSwarmUpdated {
		copy.Payload = map[string]any{"schemaVersion": 1}
		raw, _ := json.Marshal(event.Payload["swarm"])
		var v team.SwarmState
		if json.Unmarshal(raw, &v) == nil {
			copy.Payload["status"] = v.Status
			copy.Payload["reason"] = v.Reason
			copy.Payload["contractHash"] = v.ContractHash
		}
	}
	return &copy
}

func (h *handler) handleSwarmRoute(method string, segments []string) bool {
	if len(segments) < 4 || segments[1] != "runs" {
		return false
	}
	action := segments[3]
	if action != "swarm" && action != "contract-proposals" && action != "evals" && action != "artifacts" {
		return false
	}
	if h.loadTeam(segments[0]) == nil {
		return true
	}
	run := h.loadRun(segments[0], segments[2])
	if run == nil {
		return true
	}
	if h.server.Teams == nil {
		h.fail(http.StatusServiceUnavailable, errorBody("团队服务未就绪"))
		return true
	}
	if action == "artifacts" && method == http.MethodGet && len(segments) == 6 && segments[5] == "diff" {
		found := false
		if run.Swarm != nil {
			for _, a := range run.Swarm.Artifacts {
				if a.ID == segments[4] {
					found = true
				}
			}
		}
		if !found {
			h.fail(404, errorBody("候选不存在"))
			return true
		}
		if h.server.Teams.SwarmRunner == nil {
			h.fail(503, errorBody("候选执行器未就绪"))
			return true
		}
		patch, truncated, err := h.server.Teams.SwarmRunner.Diff(h.request.Context(), segments[4])
		if err != nil {
			h.fail(409, errorBody("候选缺失或已变化，请重新核查"))
			return true
		}
		h.output(200, goalObject(map[string]any{"artifactId": segments[4], "diff": patch, "truncated": truncated}), contentTypeJSON, "")
		return true
	}
	if method == http.MethodGet && len(segments) == 4 && (action == "swarm" || action == "evals") {
		if action == "evals" {
			rows := []team.SwarmEval{}
			if run.Swarm != nil {
				rows = run.Swarm.Evals
			}
			h.output(200, goalObject(map[string]any{"evals": rows}), contentTypeJSON, "")
		} else {
			h.output(200, h.swarmProject(run), contentTypeJSON, "")
		}
		return true
	}
	if method != http.MethodPost {
		h.fail(404, errorBody("页面不存在"))
		return true
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(400, errorBody(err.Error()))
		return true
	}
	var request team.SwarmRequest
	if err = json.Unmarshal(jsoncompat.EncodeCompact(value), &request); err != nil {
		h.fail(400, errorBody("Swarm 请求格式无效"))
		return true
	}
	var result *team.Run
	switch {
	case action == "contract-proposals" && len(segments) == 4:
		result, err = h.server.Teams.ProposeSwarmContract(run.ID, request)
	case action == "contract-proposals" && len(segments) == 6 && segments[5] == "accept":
		result, err = h.server.Teams.AcceptSwarmContract(run.ID, segments[4], request)
	case action == "swarm" && len(segments) == 5 && segments[4] == "evaluate":
		result, err = h.server.Teams.RequestSwarmEval(run.ID, "", request)
	case action == "swarm" && len(segments) == 5 && segments[4] == "integrate":
		result, err = h.server.Teams.RequestSwarmIntegration(run.ID, request)
	case action == "evals" && len(segments) == 6 && segments[5] == "recheck":
		result, err = h.server.Teams.RequestSwarmEval(run.ID, segments[4], request)
	default:
		h.fail(404, errorBody("页面不存在"))
		return true
	}
	if err != nil {
		switch {
		case errors.Is(err, team.ErrSwarmInvalid), errors.Is(err, team.ErrRunContentMismatch):
			h.fail(400, errorBody(err.Error()))
		default:
			h.failTeamError(err)
		}
		return true
	}
	h.output(200, h.teamRunDetail(result), contentTypeJSON, "")
	return true
}

// 不把本地候选的绝对目录、完整清单或评测私有环境发送给浏览器。
func (h *handler) swarmProject(run *team.Run) any {
	if run.Swarm == nil {
		return nil
	}
	raw, _ := json.Marshal(run.Swarm)
	parsed, _ := jsoncompat.Unmarshal(raw)
	object := parsed.(*jsoncompat.Object)
	object.Delete("operations")
	object.Set("runVersion", jsoncompat.Num(int(run.Version)))
	if contract, ok := object.MustGet("contract").(*jsoncompat.Object); ok {
		if criteria, ok := contract.MustGet("criteria").(*jsoncompat.Array); ok {
			for _, raw := range criteria.Items {
				c, ok := raw.(*jsoncompat.Object)
				if !ok {
					continue
				}
				id := stringField(c, "id")
				state := ""
				for _, record := range run.Goals {
					if record == nil || record.EvidenceHash != team.GoalEvidenceHash(run) {
						continue
					}
					for _, criterion := range record.Contract.Criteria {
						if criterion.ID != id {
							continue
						}
						if state != "" && state != criterion.State {
							state = "unknown"
						} else {
							state = criterion.State
						}
					}
				}
				if state != "" {
					c.Set("state", state)
				}
			}
		}
	}
	if h.server.Teams != nil {
		object.Set("budget", goalObject(h.server.Teams.SwarmBudget(run)))
	}
	if rows, ok := object.MustGet("artifacts").(*jsoncompat.Array); ok {
		for _, v := range rows.Items {
			a, ok := v.(*jsoncompat.Object)
			if !ok {
				continue
			}
			id, _ := a.MustGet("reviewTaskId").(string)
			for _, task := range run.Tasks {
				if task.ID != id {
					continue
				}
				verdict := "unknown"
				if task.ResultDetail != nil {
					verdict = task.ResultDetail.ReviewVerdict
				}
				a.Set("review", goalObject(map[string]any{"verdict": verdict, "independent": team.SwarmReviewIndependent(run, stringField(a, "id")), "validity": map[bool]string{true: "stale", false: "fresh"}[task.ReviewVerdictStale]}))
			}
		}
	}
	return object
}
