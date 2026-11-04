package httpd

// teams_lead_context.go 实现团队主控（Lead）会话的上下文管理入口：
// POST /api/teams/:teamId/lead-context。只动 Lead 一人的绑定，其他成员、
// 角色、Run 历史与原会话本体一概不动。
//
//	action=new    ——创建空白主控会话并换绑（懒物化 Agent 异步预热）；
//	action=rebind ——把网关转录压缩（同 Agent migrate）产出的新会话换绑为主控。
//
// 共同防线（头脑风暴收敛的结论）：
//	团队级互斥     ——非终态 Run、主控排队/未确认消息、主控运行中回合任一存在即 409；
//	版本与预期校验 ——version 必须等于账本版本，expectedSessionId 必须仍是当前
//	                 主控绑定；创建/压缩完成后的提交前再复查一次，竞态留给
//	                 SaveTeam 的乐观版本收口；
//	幂等           ——new 以 Creations 账本按请求 id 去重（与 migrate 同一账本）；
//	                 rebind 对「已是目标会话」直接返回现状。
// 原生 /compact 不在此入口：它不换绑，由前端以 teamMode:"direct" 走普通发送。

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

// handleTeamLeadContext 处理 POST /api/teams/:teamId/lead-context。
func (h *handler) handleTeamLeadContext(teamID string) {
	teamRow := h.loadTeam(teamID)
	if teamRow == nil {
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	requestID := stringField(body, "id")
	if _, parseErr := uuid.Parse(requestID); parseErr != nil {
		h.fail(http.StatusBadRequest, errorBody("请求标识无效"))
		return
	}
	version, versionErr := parseTeamVersion(body)
	if versionErr != nil {
		h.fail(http.StatusBadRequest, errorBody(versionErr.Error()))
		return
	}
	expected := stringField(body, "expectedSessionId")
	if expected == "" {
		h.fail(http.StatusBadRequest, errorBody("缺少主控会话标识，请刷新后重试"))
		return
	}
	action := stringField(body, "action")
	// Creations 重放（同请求 id 的新建重试）不受版本门拦截：上次换绑成功后
	// 版本已自增，重放应当原样收敛到同一结果，而不是被 409 挡住。
	replay := action == "new" && h.server.Creations != nil && h.server.Creations.lookup(requestID) != nil
	if !replay && version != teamRow.Version {
		h.fail(http.StatusConflict, errorBody("版本冲突，请刷新后重试"))
		return
	}
	targetID := ""
	switch action {
	case "new":
		targetID, err = h.createLeadSession(teamRow, expected, requestID)
	case "rebind":
		targetID = stringField(body, "sessionId")
		if targetID == "" {
			err = &teamInputError{http.StatusBadRequest, "缺少目标会话标识"}
		}
	default:
		err = &teamInputError{http.StatusBadRequest, "未知的主控上下文操作：" + action}
	}
	if err != nil {
		var inputErr *teamInputError
		if errors.As(err, &inputErr) {
			h.fail(inputErr.status, errorBody(inputErr.message))
		} else {
			h.fail(http.StatusConflict, errorBody(err.Error()))
		}
		return
	}

	// 提交前二次检查：压缩/创建可能耗时，期间用户可能换了主控或发起新 Run。
	fresh := h.server.TeamStore.Team(teamID)
	if fresh == nil {
		h.fail(http.StatusNotFound, errorBody("团队不存在，请刷新列表"))
		return
	}
	// 幂等收敛先行：目标已是当前主控绑定（上次换绑成功、响应丢失）时直接
	// 返回现状，不再被「主控已变更」或成员占用校验挡住。
	if teamLeadSessionID(fresh) == targetID {
		h.outputLeadContext(action, targetID, fresh)
		return
	}
	if guardErr := h.leadContextGuard(fresh, expected); guardErr != nil {
		h.fail(http.StatusConflict, errorBody(guardErr.Error()))
		return
	}
	if action == "rebind" {
		if rebindErr := h.validateLeadRebind(fresh, targetID); rebindErr != nil {
			h.fail(http.StatusBadRequest, errorBody(rebindErr.Error()))
			return
		}
	}
	kind, _ := h.server.teamMemberKind(targetID)
	// rebind（网关压缩）同样继承旧主控设置；/new 的继承在创建点完成。
	if action == "rebind" && kind != "" {
		if manager := h.server.acpAgentSet()[kind]; manager != nil {
			h.inheritSessionSettings(manager, expected, manager.Get(targetID))
		}
	}
	for i := range fresh.Members {
		if fresh.Members[i].ID != fresh.LeadID {
			continue
		}
		fresh.Members[i].SessionID = targetID
		fresh.Members[i].AgentKind = kind
		break
	}
	fresh.UpdatedAt = time.Now()
	if saveErr := h.server.TeamStore.SaveTeam(fresh); saveErr != nil {
		h.failTeamStoreError(saveErr)
		return
	}
	h.outputLeadContext(action, targetID, fresh)
}

func (h *handler) outputLeadContext(action, sessionID string, teamRow *team.Team) {
	result := jsoncompat.NewObject()
	result.Set("action", action)
	result.Set("sessionId", sessionID)
	result.Set("team", h.teamProject(teamRow))
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// leadContextGuard 校验主控上下文操作的前置静止状态。Get 只查 live 句柄，
// 绝不为了校验拉起 Agent 进程。
func (h *handler) leadContextGuard(teamRow *team.Team, expectedSessionID string) error {
	if current := teamLeadSessionID(teamRow); current != expectedSessionID {
		return errors.New("主控会话已变更，请刷新后重试")
	}
	for _, run := range h.server.TeamStore.RunsByTeam(teamRow.ID) {
		if !teamRunTerminal(run.Status) {
			return errors.New("团队仍有未结束的 Run，请先暂停或等待完成后再操作主控上下文")
		}
	}
	if h.server.Queue != nil {
		for _, entry := range h.server.Queue.threadEntries(expectedSessionID) {
			if entry.Status == SubQueued || entry.Status == SubUnknown {
				return errors.New("主控仍有排队或未确认的消息，请先处理后再操作主控上下文")
			}
		}
	}
	if kind, ok := h.server.teamMemberKind(expectedSessionID); ok {
		if manager := h.server.acpAgentSet()[kind]; manager != nil {
			if session := manager.Get(expectedSessionID); session != nil {
				snapshot := session.Snapshot()
				if snapshot.MustGet("status") == "active" {
					return errors.New("主控正在运行，请等待回合结束")
				}
				if requests, ok := snapshot.MustGet("requests").(*jsoncompat.Array); ok && len(requests.Items) > 0 {
					return errors.New("主控有待处理的请求，请先回应")
				}
			}
		}
	}
	return nil
}

// createLeadSession 为 /new 创建空白主控会话：同 Agent、团队项目目录，懒物化
// Agent 异步预热（与成员恢复同一预热通道）。Creations 账本按请求 id 去重：
// 重试（含响应丢失后的重放）不会创建第二个会话，也不重跑静止校验——上次
// 换绑可能已成功，收敛由调用方的幂等分支完成。返回可绑定的会话 id。
func (h *handler) createLeadSession(teamRow *team.Team, expectedSessionID, requestID string) (string, error) {
	kind, ok := h.server.teamMemberKind(expectedSessionID)
	if !ok {
		return "", &teamInputError{http.StatusConflict, "当前主控会话不可用，请刷新后重试"}
	}
	manager := h.server.acpAgentSet()[kind]
	if manager == nil {
		return "", &teamInputError{http.StatusConflict, "Agent " + kind + " 未配置"}
	}
	title := "团队主控 · " + teamRow.Name
	if runes := []rune(title); len(runes) > 120 {
		title = string(runes[:120])
	}
	projectKey := "team:" + teamRow.ID
	ledger := h.server.Creations
	if ledger != nil {
		if entry := ledger.lookup(requestID); entry != nil {
			if entry.Agent != kind || entry.Project != projectKey || entry.Title != title {
				return "", &teamInputError{http.StatusBadRequest, "同一创建请求不能用于不同内容"}
			}
			if entry.ID == "" {
				return "", &teamInputError{http.StatusConflict, "上次创建结果尚不确定，请先刷新聊天列表后重试"}
			}
			return entry.ID, nil
		}
	}
	if guardErr := h.leadContextGuard(teamRow, expectedSessionID); guardErr != nil {
		return "", guardErr
	}
	cwd := h.teamProjectCwd(teamRow)
	if cwd == "" {
		cwd = (teamSessionResolver{server: h.server}).Cwd(expectedSessionID)
	}
	if cwd == "" {
		return "", &teamInputError{http.StatusConflict, "无法确定团队项目目录，请稍后重试"}
	}
	if ledger != nil {
		ledger.begin(requestID, &creationEntry{Agent: kind, Project: projectKey,
			Title: title, Host: "local", At: ledger.now(), State: "creating"})
	}
	session, createErr := manager.Create("sess_"+uuid.NewString(), cwd, title, 60*time.Second)
	if createErr != nil {
		log.Printf("主控新会话创建失败（team=%s）: %v", teamRow.ID, createErr)
		return "", &teamInputError{http.StatusConflict, createErr.Error()}
	}
	if ledger != nil {
		ledger.complete(requestID, session.ID)
	}
	warmupMemberSession(manager, session)
	h.inheritSessionSettings(manager, expectedSessionID, session)
	return session.ID, nil
}

// inheritSessionSettings 把源会话的模型与推理强度应用到新会话：团队主控换绑
// 与普通会话 /new 共用；
// 不应把用户选过的设置悄悄重置回 Agent 默认。尽力而为——旧会话不在线（拿
// 不到当前设置）或新会话拒绝该设置时只记日志，绝不阻断换绑；settings 的
// effort 词表是网关侧词汇（UpdateSettings 经 EffortMap 映射），View 里存的
// 是 Agent 侧值，必须先反查。
func (h *handler) inheritSessionSettings(manager *acpruntime.Manager, oldSessionID string, fresh *acpruntime.Session) {
	old := manager.Get(oldSessionID)
	if old == nil || fresh == nil {
		return
	}
	snapshot := old.Snapshot()
	model, _ := snapshot.MustGet("model").(string)
	thinking, _ := snapshot.MustGet("effort").(string)
	if model == "" && thinking == "" {
		return
	}
	effort := ""
	for word, mapped := range manager.EffortMap {
		if mapped == thinking {
			effort = word
			break
		}
	}
	if _, err := manager.UpdateSettings(fresh, model, effort, nil); err != nil {
		log.Printf("主控设置继承未完成（%s → %s）: %v", oldSessionID, fresh.ID, err)
	}
}

// validateLeadRebind 校验 rebind 的目标会话：属于本网关的 ACP 会话、未被其他
// 成员占用、项目团队还必须仍在同一项目目录（引用失效保留记录，不静默改派）。
func (h *handler) validateLeadRebind(teamRow *team.Team, sessionID string) error {
	if !teamIDPattern.MatchString(sessionID) {
		return errors.New("目标会话标识无效")
	}
	if holder := teamSessionHolder(teamRow, sessionID); holder != "" {
		return fmt.Errorf("会话已被成员 %s 绑定，一个会话同一时间只能属于一个成员", holder)
	}
	if _, ok := h.server.teamMemberKind(sessionID); !ok {
		return errors.New("阶段一 Team 成员仅支持 ACP Agent 会话，请从会话列表选择")
	}
	if teamRow.Project != "" {
		if projectErr := h.validateTeamSessionProject(teamRow.Project, sessionID); projectErr != nil {
			return projectErr
		}
	}
	return nil
}
