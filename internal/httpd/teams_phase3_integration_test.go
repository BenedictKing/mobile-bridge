package httpd

// teams_phase3_integration_test.go 是 §12 阶段三验收级集成测试（docs/
// team-orchestration-plan.md §12 阶段三验收、§13.2 条目 10/13/14、§13.3
// 条目 9 的可测子集）：全部走生产链路——真实 team.Service + NewTeamAdapters
// 的 submissions 账本 + acpruntime 伪 Agent（teams_integration_test.go 的
// teamAgent，本阶段扩展了 REPLYACTIONS 动作剧本 / ASKQ 反向提问卡 /
// actionsrepair 修复剧本三个分支）。覆盖：
//
//  1. §12 验收主线：autoPlan single Run 的 Lead 回复合法 delegate（reviewer/
//     tester，另加一条给 Lead 的第二轮载体 delegate——实现里 summarize 动作
//     只能来自后续 Lead 任务回复的再解析，单块内的 summarize 会在同一次
//     passActions 里立即执行、依赖只含根任务，拿不到两份 delegate 结果）→
//     派生任务按序执行 → summarize 派生任务收到两来源摘要 → Run 完成；
//  2. §13.2:10 非法动作闭环：invalid_action_block + 修复指令回送 Lead 会话 →
//     修复后执行；连续 3 次修复失败 final 收场、无第 4 条修复消息、Run 不因
//     动作失败而失败；宽容 JSON（单引号/尾逗号）一次通过；修复 ID 超长
//     （预置长 sourceTaskID）确定性 final；
//  3. §12「拒绝可解释」：预设 proposed 动作走真实 passActions——未知成员、
//     超轮次、派生 ID 超长、任务数达上限各落 action_rejected 事件 + 快照
//     status=rejected/reason 非空（前端数据源）；autoDispatch=false 的
//     delegate 停在 proposed（§11 动作预览，不执行也不拒绝）且不阻塞终局；
//  4. §13.2:14 escalate：ASKQ 提问卡登记 pendingQuestions → GET questions →
//     decision 选项无效 400 保持 open / 版本过期 409 / 有效回答送达成员会话、
//     turn 继续、问题 answered / 同 decisionID 重放 duplicate / 异 decisionID
//     二次回答 409 / 开放问题期间任务超时豁免（taskTimeoutSecs=1 跨两个超时
//     窗口仍 running）；ASKQ4 的 3 上限与额度释放、取消后 withdrawn 再答 409；
//  5. §13.2:13 amend：3 任务 pipeline 修订末任务——前缀 inputHash 命中
//     task_cache_hit×2 直落 completed 零重发、末任务真执行、父 Run journal
//     零增、parentRunId/cacheImports 投影、同 id 重放 duplicate、异内容 400、
//     头任务异哈希断链、依赖变化断链、插入新 exclusive 任务前缀截断；
//  6. 恢复面：预置 proposed 动作/pendingQuestions/缓存任务的 Run 同 dataDir
//     重装配 + RecoverAll——动作被 loop 补执行（journal 同 ActionKey 仅一条
//     action_proposed）、问题复原且会话侧过期撤回、缓存任务保持 completed
//     不重发；team-runs.json 损坏后按 journal 重建 pendingQuestions/
//     pendingActions（含 rejected reason）/缓存完成/Run.Round 全在；
//  7. §13.3:9 input_hash_mismatch 可见：快照哈希与派发记录漂移 → 事件 +
//     waiting_input。
//
// 纪律同阶段一/二组：等待全部 deadline 轮询（120ms 节拍、20s 超时，超时窗口
// 类断言只等时钟跨线、不用固定 sleep 判定正确性）；SetWaitSlice 在装配期已
// 缩到毫秒级；缺 python3 时沿用 t.Skip 惯例。选择器：`-run TestTeamPhase3`。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

// ---- 共用小机械 ----

// writeAgentScriptFile 落伪 Agent 的动作/修复剧本文件（actions-*.txt /
// actionsrepair-*.txt；teamAgent 的 REPLYACTIONS/修复分支按名读取）。
func writeAgentScriptFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readAgentAnswers 解析 answers.log（ask-<id>\x00<outcome json>）。
func readAgentAnswers(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "answers.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}
		}
		t.Fatal(err)
	}
	answers := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		key, outcome, _ := strings.Cut(line, "\x00")
		answers[key] = outcome
	}
	return answers
}

// phase3Questions 取 RunDetail 顶层 pendingQuestions（缺失为空表）。
func phase3Questions(t *testing.T, detail map[string]any) []map[string]any {
	t.Helper()
	rows, _ := detail["pendingQuestions"].([]any)
	questions := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if question, ok := row.(map[string]any); ok {
			questions = append(questions, question)
		}
	}
	return questions
}

// phase3Actions 取 RunDetail 顶层 pendingActions。
func phase3Actions(t *testing.T, detail map[string]any) []map[string]any {
	t.Helper()
	rows, _ := detail["pendingActions"].([]any)
	actions := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if action, ok := row.(map[string]any); ok {
			actions = append(actions, action)
		}
	}
	return actions
}

// phase3FindQuestion 按 questionId 取问题条目（无则 nil）。
func phase3FindQuestion(t *testing.T, detail map[string]any, questionID string) map[string]any {
	t.Helper()
	for _, question := range phase3Questions(t, detail) {
		if question["questionId"] == questionID {
			return question
		}
	}
	return nil
}

// phase3OpenQuestionCount 统计指定 Task 的 open 问题数。
func phase3OpenQuestionCount(t *testing.T, detail map[string]any, taskID string) int {
	t.Helper()
	count := 0
	for _, question := range phase3Questions(t, detail) {
		if question["taskId"] == taskID && question["status"] == "open" {
			count++
		}
	}
	return count
}

// eventPayloadString / eventPayloadInt 是 journal payload 的宽松取值
// （journal 重放经 jsoncompat，数字是 json.Number）。
func eventPayloadString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func eventPayloadInt(payload map[string]any, key string) int {
	switch value := payload[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		number, err := value.Int64()
		if err != nil {
			return 0
		}
		return int(number)
	}
	return 0
}

// runEventCount 统计指定 Run 的某类型事件条数。
func runEventCount(events []*team.Event, runID, eventType string) int {
	count := 0
	for _, event := range events {
		if event != nil && event.RunID == runID && event.Type == eventType {
			count++
		}
	}
	return count
}

// createTeamWithPolicy 创建带策略片段的团队（autoDispatch 等执行闸门只能经
// policy 下发，createTeamWithMembers 不带 policy）。
func (e *teamIntegrationEnv) createTeamWithPolicy(t *testing.T, policy, membersJSON string) string {
	t.Helper()
	response := e.request(http.MethodPost, "/api/teams",
		`{"name":"阶段三策略团队","policy":`+policy+`,"members":`+membersJSON+`}`)
	if response.Code != http.StatusOK {
		t.Fatalf("创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// decide POST decision（response 是完整 JSON 对象字面量）。
func (e *teamIntegrationEnv) decide(t *testing.T, teamID, runID, decisionID, questionID string, version uint64, response string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"decisionId":%q,"questionId":%q,"version":%d,"response":%s}`,
		decisionID, questionID, version, response)
	return e.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/decision", body)
}

// amend POST amend 并解包响应 JSON（非 200 时 page 为空，错误文案看 body）。
func (e *teamIntegrationEnv) amend(t *testing.T, teamID, runID, body string) (map[string]any, int, string) {
	t.Helper()
	response := e.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/amend", body)
	page := map[string]any{}
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	return page, response.Code, response.Body.String()
}

// ---- 用例 1：§12 验收主线——autoPlan Run 的 delegate×2 → 按序执行 → 摘要交回 Lead ----

// TestTeamPhase3AutoPlanDelegatesAndSummarize：空计划（autoPlan）single Run，
// Lead 首条回复带三条合法 delegate（reviewer / tester / lead——第三条是第二轮
// 载体：其回复经同一 completed 分支再解析产生 summarize 动作；实现里 summarize
// 的依赖取执行时刻的全部已完成任务，单块内混排会在 delegate 完成前执行、拿
// 不到两份结果）。断言：两条成员 delegate 的 action_proposed 与根任务
// task_completed 同批落账（journal 序号连续）；派生任务按写锁序串行执行；
// summarize 派生任务（round 2）收到两份 delegate 结果（prompts.log）；Run
// 完成且 Round 推进到 2；完成后驱动若干节拍无重复派发/重复 action_proposed。
func TestTeamPhase3AutoPlanDelegatesAndSummarize(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionLead := env.createMemberSession(t, "lead-sess", "主线 Lead")
	sessionRev := env.createMemberSession(t, "rev-sess", "审阅成员")
	sessionTest := env.createMemberSession(t, "test-sess", "测试成员")
	// maxParallelTasks=1：ReadyTasks 单 pass 会同时放行多条无依赖派生任务，
	// 派发池对写锁的竞争跨会话不定序；并发 1 使派生任务严格按 ordinal 串行
	//（R→T→载体→summarize），summarize 的依赖集（执行时刻的全部已完成任务）
	// 才确定性包含两份 delegate 结果。
	response := env.request(http.MethodPost, "/api/teams", `{"name":"阶段三主线团队","policy":{"autoDispatch":true,"maxParallelTasks":1},
		"members":[{"id":"lead","sessionId":"`+sessionLead+`","role":"lead"},
		{"id":"reviewer","sessionId":"`+sessionRev+`","role":"member"},
		{"id":"tester","sessionId":"`+sessionTest+`","role":"member"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	teamID := created.ID

	writeAgentScriptFile(t, env.dataDir, "actions-MAIN.txt",
		"<team-actions>\n{\"actions\":[\n"+
			`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"reviewer","task":"审阅主线产出 PH3-REVIEW"},`+"\n"+
			`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"tester","task":"测试主线产出 PH3-TEST"},`+"\n"+
			`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"lead","task":"REPLYACTIONS:SUMM 两份产出到齐后请输出最终汇总"}`+"\n"+
			"]}\n</team-actions>")
	writeAgentScriptFile(t, env.dataDir, "actions-SUMM.txt",
		"<team-actions>\n{\"actions\":[{\"type\":\"summarize\",\"sourceTaskId\":\"lead-d1-2\",\"round\":2}]}\n</team-actions>")

	runID := env.createRun(t, teamID, `{"goal":"REPLYACTIONS:MAIN 验收主线：分派审阅与测试后汇总","mode":"single"}`)
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("主线 Run 未在时限内完成")
	}

	detail := env.runDetail(t, teamID, runID)
	if detail["autoPlan"] != true {
		t.Fatalf("空计划 Run 应 autoPlan=true: %v", detail["autoPlan"])
	}
	if round := detail["round"]; round != float64(2) {
		t.Fatalf("round-2 summarize 执行后 Run.Round 应推进到 2: %v", round)
	}
	// 派生任务齐备且全部完成（根 + reviewer/tester/lead 三 delegate + summarize）。
	for _, taskID := range []string{"lead", "lead-d1-0", "lead-d1-1", "lead-d1-2", "lead-d1-2-s2-0"} {
		if task := teamTaskOf(t, detail, taskID); task["status"] != "completed" {
			t.Fatalf("任务 %s 应 completed: %v", taskID, task["status"])
		}
	}
	if got := len(detail["tasks"].([]any)); got != 5 {
		t.Fatalf("应恰有 5 个任务（根+3 delegate+summarize）: %d", got)
	}
	// 动作全部 executed（两条成员 delegate + 载体 delegate + summarize）。
	actions := phase3Actions(t, detail)
	if len(actions) != 4 {
		t.Fatalf("应恰有 4 条动作: %v", actions)
	}
	for _, action := range actions {
		if action["status"] != "executed" {
			t.Fatalf("动作应全部 executed: %v", action)
		}
	}

	// 两条成员 delegate 的 action_proposed 与根任务 task_completed 同批：
	// journal 序号连续（completed 后紧跟三条 proposal）。
	events := env.journalEvents(t)
	completedSeq := int64(-1)
	for _, event := range events {
		if event.Type == team.EventTaskCompleted && eventPayloadString(event.Payload, "taskId") == "lead" {
			completedSeq = int64(event.Sequence)
			break
		}
	}
	if completedSeq < 0 {
		t.Fatal("未找到根任务 task_completed")
	}
	var proposeSeqs []int64
	for _, event := range events {
		if event.Type != team.EventActionProposed {
			continue
		}
		if eventPayloadString(event.Payload, "sourceTaskId") == "lead" && eventPayloadInt(event.Payload, "round") == 1 {
			proposeSeqs = append(proposeSeqs, int64(event.Sequence))
		}
	}
	if len(proposeSeqs) != 3 {
		t.Fatalf("根任务结算批应恰带 3 条 action_proposed: %v", proposeSeqs)
	}
	for i, seq := range proposeSeqs {
		if seq != completedSeq+int64(i)+1 {
			t.Fatalf("action_proposed 应与 task_completed 同批连续: completed=%d propose=%v", completedSeq, proposeSeqs)
		}
	}
	if count := teamEventCount(events, team.EventActionProposed); count != 4 {
		t.Fatalf("全程应恰 4 条 action_proposed（3+summarize 1）: %d", count)
	}
	// 派生任务的 task_planned 携带 actionKey（重建据此置 executed）。
	plannedWithKey := 0
	for _, event := range events {
		if event.Type == team.EventTaskPlanned && eventPayloadString(event.Payload, "actionKey") != "" {
			plannedWithKey++
		}
	}
	if plannedWithKey != 4 {
		t.Fatalf("4 条派生任务的 task_planned 都应带 actionKey: %d", plannedWithKey)
	}

	// 提示词严格串行（写锁互斥）：根 → reviewer → tester → lead 载体 → summarize。
	records := readTeamPrompts(t, env.dataDir)
	if len(records) != 5 {
		t.Fatalf("应恰 5 条提示词: %d", len(records))
	}
	for _, needle := range []string{"[Team delegation]", "id=reviewer", "id=tester", "team-actions", "验收主线"} {
		if !strings.Contains(records[0].text, needle) {
			t.Fatalf("首个 Lead 提示词应包含指派简报 %q:\n%s", needle, records[0].text)
		}
	}
	revAt := promptIndex(records, "PH3-REVIEW")
	testAt := promptIndex(records, "PH3-TEST")
	sumCarrierAt := promptIndex(records, "REPLYACTIONS:SUMM")
	summAt := promptIndex(records, "请汇总上述全部已完成任务的结果")
	if revAt != 1 || testAt != 2 || sumCarrierAt != 3 || summAt != 4 {
		t.Fatalf("派发顺序应为 根→reviewer→tester→载体→summarize: rev=%d test=%d carrier=%d summ=%d",
			revAt, testAt, sumCarrierAt, summAt)
	}
	if records[revAt].session != sessionRev || records[testAt].session != sessionTest {
		t.Fatalf("delegate 应落在目标成员会话: %v", records)
	}
	for _, index := range []int{revAt, testAt} {
		if strings.Contains(records[index].text, "[Team delegation]") {
			t.Fatalf("普通成员任务不得携带 Lead 简报:\n%s", records[index].text)
		}
	}
	// summarize 收到两来源摘要（§12「摘要交回 Lead」）。
	summarizeText := records[summAt].text
	for _, needle := range []string{"[Team delegation]", "[Team context]", "来源任务：lead-d1-0", "PH3-REVIEW",
		"来源任务：lead-d1-1", "PH3-TEST", "[Assigned task]"} {
		if !strings.Contains(summarizeText, needle) {
			t.Fatalf("summarize 派发文本应含 %q:\n%s", needle, summarizeText)
		}
	}
	if records[summAt].session != sessionLead {
		t.Fatalf("summarize 应交回 Lead 会话: %v", records[summAt].session)
	}

	// 重复驱动不重派、不重复提案（重复 ActionKey 不再建任务的可观测面）。
	baselinePrompts := len(readTeamPrompts(t, env.dataDir))
	baselineProposed := teamEventCount(env.journalEvents(t), team.EventActionProposed)
	env.drive(t, 1200*time.Millisecond, func() bool { return false })
	if got := len(readTeamPrompts(t, env.dataDir)); got != baselinePrompts {
		t.Fatalf("完成后不应再派发: %d ≠ %d", got, baselinePrompts)
	}
	if got := teamEventCount(env.journalEvents(t), team.EventActionProposed); got != baselineProposed {
		t.Fatalf("完成后不应再产生 action_proposed: %d ≠ %d", got, baselineProposed)
	}
}

// ---- 用例 2：§13.2:10 非法动作闭环 ----

// TestTeamPhase3InvalidActionRepairLoop 覆盖三条修复回路：非法 JSON → 修复
// 指令回送 Lead 会话 → 修复回合回合法块 → 派生任务执行；连续 3 次修复仍
// 非法 → 第 4 次失败 final=true、无第 4 条修复消息、Run 不因动作失败而失败
// （根结果按普通文本保留）；宽容 JSON（单引号/尾逗号）一次通过（无
// invalid_action_block、无修复回合）。
func TestTeamPhase3InvalidActionRepairLoop(t *testing.T) {
	t.Run("非法后修复合法并执行", func(t *testing.T) {
		env := newTeamIntegrationEnv(t, t.TempDir())
		sessionLead := env.createMemberSession(t, "lead-sess", "修复 Lead")
		sessionDev := env.createMemberSession(t, "dev-sess", "修复执行成员")
		teamID := env.createTeamWithPolicy(t, `{"autoDispatch":true}`, `[{"id":"lead","sessionId":"`+sessionLead+
			`","role":"lead"},{"id":"dev","sessionId":"`+sessionDev+`","role":"member"}]`)
		writeAgentScriptFile(t, env.dataDir, "actions-BAD.txt",
			"<team-actions>{\"actions\":[}</team-actions>")
		writeAgentScriptFile(t, env.dataDir, "actionsrepair-1.txt",
			"<team-actions>\n{\"actions\":[{\"type\":\"delegate\",\"sourceTaskId\":\"actfix-lead-1\",\"round\":1,\"member\":\"dev\",\"task\":\"修复后执行任务 PH3-REPAIRED\"}]}\n</team-actions>")
		runID := env.createRun(t, teamID, `{"goal":"REPLYACTIONS:BAD 修复回路验收","mode":"single"}`)
		if !env.drive(t, 20*time.Second, func() bool {
			return env.runDetail(t, teamID, runID)["status"] == "completed"
		}) {
			t.Fatal("修复回路 Run 未在时限内完成")
		}
		// 修复指令回送 Lead 会话：prompts.log 里 Lead 会话收到含 violation 的修复提示词。
		records := readTeamPrompts(t, env.dataDir)
		repairAt := -1
		for i, record := range records {
			if record.session == sessionLead && strings.Contains(record.text, "第 1 次解析失败") {
				repairAt = i
			}
		}
		if repairAt < 0 {
			t.Fatalf("Lead 会话应收到 violation 修复指令: %v", records)
		}
		if text := records[repairAt].text; !strings.Contains(text, "请重新输出修正后的动作块") ||
			!strings.Contains(text, "不是合法 JSON") {
			t.Fatalf("修复提示词应携带 violation 列表与修复指引: %q", text)
		}
		// 修复成功后动作执行：invalid 恰一次（非 final），派生任务在 dev 会话完成。
		events := env.journalEvents(t)
		invalid := teamEventPayload(events, team.EventInvalidActionBlock)
		if invalid == nil || invalid["final"] != false || eventPayloadInt(invalid, "attempt") != 1 {
			t.Fatalf("应恰一次非 final 的 invalid_action_block(attempt=1): %v", invalid)
		}
		if count := teamEventCount(events, team.EventInvalidActionBlock); count != 1 {
			t.Fatalf("修复成功后不应再有 invalid_action_block: %d", count)
		}
		if count := teamEventCount(events, team.EventActionProposed); count != 1 {
			t.Fatalf("修复回合应恰产生 1 条 action_proposed: %d", count)
		}
		detail := env.runDetail(t, teamID, runID)
		derived := teamTaskOf(t, detail, "actfix-lead-1-d1-0")
		if derived["status"] != "completed" || derived["result"] == nil {
			t.Fatalf("修复后的派生任务应完成: %v", derived)
		}
		derivedAt := promptIndex(records, "PH3-REPAIRED")
		if derivedAt < 0 || records[derivedAt].session != sessionDev {
			t.Fatalf("派生任务应派发到 dev 会话: %v", records)
		}
		if len(records) != 4 || promptIndex(records, "请汇总本次团队执行成果") < 0 {
			t.Fatalf("应为根/修复/派生/最终收尾共 4 条提示词: %v", records)
		}
	})

	t.Run("三次修复失败后 final 收场", func(t *testing.T) {
		env := newTeamIntegrationEnv(t, t.TempDir())
		sessionLead := env.createMemberSession(t, "lead-sess", "连续失败 Lead")
		teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionLead+`","role":"lead"}]`)
		writeAgentScriptFile(t, env.dataDir, "actions-BAD3.txt", "<team-actions>not-json-at-all</team-actions>")
		for attempt := 1; attempt <= 3; attempt++ {
			writeAgentScriptFile(t, env.dataDir, "actionsrepair-"+strconv.Itoa(attempt)+".txt",
				fmt.Sprintf("<team-actions>{broken %d</team-actions>", attempt))
		}
		runID := env.createRun(t, teamID, `{"goal":"REPLYACTIONS:BAD3 连续失败验收","mode":"single"}`)
		if !env.drive(t, 20*time.Second, func() bool {
			return env.runDetail(t, teamID, runID)["status"] == "completed"
		}) {
			t.Fatal("三次修复失败后 Run 应确定性收场（completed，不因动作失败而失败）")
		}
		events := env.journalEvents(t)
		if count := teamEventCount(events, team.EventInvalidActionBlock); count != 4 {
			t.Fatalf("应恰 4 次 invalid_action_block（3 次修复 + final）: %d", count)
		}
		finalCount := 0
		for _, event := range events {
			if event.Type != team.EventInvalidActionBlock {
				continue
			}
			if event.Payload["final"] == true {
				finalCount++
				if eventPayloadInt(event.Payload, "attempt") != 4 {
					t.Fatalf("final 应发生在第 4 次失败: %v", event.Payload)
				}
				violations, _ := event.Payload["violations"].([]any)
				if len(violations) == 0 {
					t.Fatalf("final 事件应携带非空 violations: %v", event.Payload)
				}
			}
		}
		if finalCount != 1 {
			t.Fatalf("应恰一次 final=true: %d", finalCount)
		}
		// 无第 4 条修复消息：修复提示词恰 3 条，任务无 actfix-lead-4。
		records := readTeamPrompts(t, env.dataDir)
		repairCount := 0
		for _, record := range records {
			if strings.Contains(record.text, "次解析失败") {
				repairCount++
			}
		}
		if repairCount != 3 || len(records) != 4 {
			t.Fatalf("应恰 3 条修复消息（共 4 条提示词）: 修复=%d 总数=%d", repairCount, len(records))
		}
		detail := env.runDetail(t, teamID, runID)
		if got := len(detail["tasks"].([]any)); got != 4 {
			t.Fatalf("任务应为 根+3 修复回合 共 4 个: %d", got)
		}
		if task := teamTaskOf(t, detail, "lead"); task["status"] != "completed" || task["result"] == nil {
			t.Fatalf("根任务结果应按普通文本保留: %v", task)
		}
		if text, _ := teamTaskOf(t, detail, "lead")["result"].(map[string]any)["result"].(string); !strings.Contains(text, "not-json-at-all") {
			t.Fatalf("根任务结果应保留原始回复文本: %v", text)
		}
	})

	t.Run("宽容解码一次通过", func(t *testing.T) {
		env := newTeamIntegrationEnv(t, t.TempDir())
		sessionLead := env.createMemberSession(t, "lead-sess", "宽容 Lead")
		sessionDev := env.createMemberSession(t, "dev-sess", "宽容执行成员")
		teamID := env.createTeamWithPolicy(t, `{"autoDispatch":true}`, `[{"id":"lead","sessionId":"`+sessionLead+
			`","role":"lead"},{"id":"dev","sessionId":"`+sessionDev+`","role":"member"}]`)
		writeAgentScriptFile(t, env.dataDir, "actions-LENIENT.txt",
			"<team-actions>{'actions': [{'type': 'delegate', 'sourceTaskId': 'lead', 'round': 1, 'member': 'dev', 'task': '宽恕解码任务 PH3-LENIENT',},]}</team-actions>")
		runID := env.createRun(t, teamID, `{"goal":"REPLYACTIONS:LENIENT 宽容解码验收","mode":"single"}`)
		if !env.drive(t, 20*time.Second, func() bool {
			return env.runDetail(t, teamID, runID)["status"] == "completed"
		}) {
			t.Fatal("宽容解码 Run 未在时限内完成")
		}
		events := env.journalEvents(t)
		if count := teamEventCount(events, team.EventInvalidActionBlock); count != 0 {
			t.Fatalf("宽容形态不应产生 invalid_action_block: %d", count)
		}
		if count := teamEventCount(events, team.EventActionProposed); count != 1 {
			t.Fatalf("宽容形态应直接执行: action_proposed=%d", count)
		}
		if task := teamTaskOf(t, env.runDetail(t, teamID, runID), "lead-d1-0"); task["status"] != "completed" {
			t.Fatalf("派生任务应完成: %v", task)
		}
		records := readTeamPrompts(t, env.dataDir)
		if len(records) != 3 || promptIndex(records, "解析失败") >= 0 || promptIndex(records, "请汇总本次团队执行成果") < 0 {
			t.Fatalf("应为根/派生/最终收尾共 3 条提示词且无修复回合: %v", records)
		}
	})
}

// writeTeamPhase3LongIDState 预置「AutoPlan Run + 56 字符 Lead 任务（planned）」
// 的账本组合：修复任务 ID actfix-{root}-1 将达 65 字符（成员 ID 上限 64），
// 首次解析失败即确定性 final、绝不创建修复回合。
func writeTeamPhase3LongIDState(t *testing.T, dataDir string) (string, string) {
	t.Helper()
	now := time.Now()
	const teamID = "tteam-p3-longid"
	const runID = "trun-p3-longid"
	const sessionLead = "km-team-1"
	longRoot := "r" + strings.Repeat("x", 55) // 56 字符：actfix-…-1 = 65 > 64。
	policy := team.DefaultPolicy()
	task := &team.Task{ID: longRoot, MemberID: "lead", Prompt: "长源任务 PH3-LONGID REPLYACTIONS:BAD9 请输出动作块",
		Ordinal: 0, Status: team.TaskPlanned, WriteLock: team.TaskWriteLockNone}
	task.InputHash = team.InputHashFor(task, team.PolicyDigest(policy), "")
	run := &team.Run{ID: runID, TeamID: teamID, Goal: "修复 ID 超长", Mode: team.ModeSingle,
		AutoPlan: true, Status: team.RunDispatching, Round: 1, PlanHash: "hand-p3-longid",
		Tasks: []team.Task{*task}, Version: 1, CreatedAt: now, UpdatedAt: now}
	writeTeamPhase3Files(t, dataDir,
		map[string]*team.Team{teamID: {ID: teamID, Name: "长 ID 团队",
			Members: []team.TeamMember{{ID: "lead", SessionID: sessionLead, AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
			LeadID:  "lead", Policy: policy, CreatedAt: now, UpdatedAt: now}},
		map[string]*team.Run{runID: run},
		[]*team.Event{
			{ID: uuid.NewString(), RunID: runID, Type: team.EventRunCreated, Source: "user",
				Payload: map[string]any{"teamId": teamID, "goal": run.Goal, "mode": team.ModeSingle, "autoPlan": true, "planHash": run.PlanHash, "taskCount": 1}, CreatedAt: now},
			{ID: uuid.NewString(), RunID: runID, Type: team.EventTaskPlanned, Source: "user",
				Payload: map[string]any{"taskId": longRoot, "memberId": "lead"}, CreatedAt: now},
		}, nil, sessionLead+"\x00"+dataDir+"\n")
	writeAgentScriptFile(t, dataDir, "actions-BAD9.txt", "<team-actions>{\"actions\": [</team-actions>")
	return teamID, runID
}

// TestTeamPhase3RepairIDOverflowFinal：修复 ID 超长（预置长 sourceTaskID）→
// 首次失败即 final=true、violation 说明 ID 超长、不创建修复任务、Run 完成。
func TestTeamPhase3RepairIDOverflowFinal(t *testing.T) {
	dataDir := t.TempDir()
	teamID, runID := writeTeamPhase3LongIDState(t, dataDir)
	env := newTeamIntegrationEnv(t, dataDir)
	if _, err := env.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	if err := env.server.TeamFeeds.PrimeFromJournal(); err != nil {
		t.Fatalf("PrimeFromJournal: %v", err)
	}
	if err := env.server.Teams.RecoverAll(); err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("超长修复 ID 的 Run 应确定性完成")
	}
	events := env.journalEvents(t)
	if count := teamEventCount(events, team.EventInvalidActionBlock); count != 1 {
		t.Fatalf("应恰一次 invalid_action_block: %d", count)
	}
	payload := teamEventPayload(events, team.EventInvalidActionBlock)
	if payload["final"] != true {
		t.Fatalf("ID 超长应直接 final: %v", payload)
	}
	violations, _ := payload["violations"].([]any)
	sawOverflow := false
	for _, violation := range violations {
		if text, _ := violation.(string); strings.Contains(text, "超长") {
			sawOverflow = true
		}
	}
	if !sawOverflow {
		t.Fatalf("violations 应说明修复任务 ID 超长: %v", payload)
	}
	detail := env.runDetail(t, teamID, runID)
	if got := len(detail["tasks"].([]any)); got != 1 {
		t.Fatalf("不应创建修复回合任务: %d", got)
	}
	if promptIndex(readTeamPrompts(t, dataDir), "解析失败") >= 0 {
		t.Fatal("超长 ID 不应产生修复消息")
	}
}

// ---- 用例 3：§12「拒绝可解释」——执行侧拒绝与动作预览 ----

// writeTeamPhase3RejectState 预置两支团队、两条带 proposed 动作的 Run（快照
// 直载 proposed 动作——真实 settle/解析路径无法在解析与执行的毫秒窗内确定性
// 改动团队/策略，执行侧拒绝的可解释面按此形态走真实 passActions）：
//   - 团队 A（autoDispatch=true、maxTasksPerRun=2）：未知成员 / 超轮次 /
//     派生 ID 超长 / 任务数达上限（summarize）四条待拒动作；
//   - 团队 B（autoDispatch=false）：一条 delegate——§11 动作预览形态（停
//     proposed、不执行不拒绝、不阻塞终局）。
func writeTeamPhase3RejectState(t *testing.T, dataDir string) (teamA, runA, teamB, runB, sessionA string) {
	t.Helper()
	now := time.Now()
	teamA, runA = "tteam-p3r-a", "trun-p3r-a"
	teamB, runB = "tteam-p3r-b", "trun-p3r-b"
	sessionA = "km-team-1"
	const sessionDevA, sessionLeadB = "km-team-2", "km-team-3"

	policyA := team.DefaultPolicy()
	policyA.AutoDispatch = true
	policyA.MaxTasksPerRun = 2
	policyB := team.DefaultPolicy() // AutoDispatch=false（默认）：动作预览。

	result := &team.TaskResult{SessionID: sessionA, TurnID: "turn-p3r", Result: "主线产出", ResultChars: 4}
	root := team.Task{ID: "root", MemberID: "lead", Prompt: "主线任务", Ordinal: 0,
		Status: team.TaskCompleted, WriteLock: team.TaskWriteLockNone, Result: "主线产出", ResultDetail: result}
	longSource := "s" + strings.Repeat("y", 61) // 62 字符：派生 ID +5 = 67 > 64。
	longTask := root
	longTask.ID = longSource
	longTask.Ordinal = 1
	runARow := &team.Run{ID: runA, TeamID: teamA, Goal: "拒绝可解释", Mode: team.ModeSingle,
		AutoPlan: true, Status: team.RunDispatching, Round: 1, PlanHash: "hand-p3r-a",
		Tasks: []team.Task{root, longTask}, Version: 1, CreatedAt: now, UpdatedAt: now}
	runARow.PendingActions = []team.Action{
		{Type: team.ActionTypeDelegate, SourceTaskID: "root", Round: 1, Member: "ghost",
			Task: "未知成员任务", ActionKey: team.ActionKey(teamA, runA, "root", 1, 0), Status: team.ActionStatusProposed},
		{Type: team.ActionTypeDelegate, SourceTaskID: "root", Round: 9, Member: "lead",
			Task: "超轮次任务", ActionKey: team.ActionKey(teamA, runA, "root", 9, 1), Status: team.ActionStatusProposed},
		{Type: team.ActionTypeDelegate, SourceTaskID: longSource, Round: 1, Member: "lead",
			Task: "超长派生任务", ActionKey: team.ActionKey(teamA, runA, longSource, 1, 2), Status: team.ActionStatusProposed},
		{Type: team.ActionTypeSummarize, SourceTaskID: "root", Round: 1,
			ActionKey: team.ActionKey(teamA, runA, "root", 1, 3), Status: team.ActionStatusProposed},
	}
	runBRow := &team.Run{ID: runB, TeamID: teamB, Goal: "动作预览", Mode: team.ModeSingle,
		AutoPlan: true, Status: team.RunDispatching, Round: 1, PlanHash: "hand-p3r-b",
		Tasks: []team.Task{root, team.Task{ID: "b-src", MemberID: "lead", Prompt: "预览来源", Ordinal: 1,
			Status: team.TaskCompleted, WriteLock: team.TaskWriteLockNone, Result: "产出", ResultDetail: result}},
		Version: 1, CreatedAt: now, UpdatedAt: now}
	runBRow.Tasks[0].ID = "root"
	runBRow.PendingActions = []team.Action{
		{Type: team.ActionTypeDelegate, SourceTaskID: "root", Round: 1, Member: "dev",
			Task: "预览动作任务", ActionKey: team.ActionKey(teamB, runB, "root", 1, 0), Status: team.ActionStatusProposed},
	}

	teams := map[string]*team.Team{
		teamA: {ID: teamA, Name: "拒绝团队",
			Members: []team.TeamMember{
				{ID: "lead", SessionID: sessionA, AgentKind: "kimi", Role: team.RoleLead, Enabled: true},
				{ID: "dev", SessionID: sessionDevA, AgentKind: "kimi", Role: team.RoleMember, Enabled: true},
			}, LeadID: "lead", Policy: policyA, CreatedAt: now, UpdatedAt: now},
		teamB: {ID: teamB, Name: "预览团队",
			Members: []team.TeamMember{
				{ID: "lead", SessionID: sessionLeadB, AgentKind: "kimi", Role: team.RoleLead, Enabled: true},
				{ID: "dev", SessionID: "km-team-4", AgentKind: "kimi", Role: team.RoleMember, Enabled: true},
			}, LeadID: "lead", Policy: policyB, CreatedAt: now, UpdatedAt: now},
	}
	events := []*team.Event{
		{ID: uuid.NewString(), RunID: runA, Type: team.EventRunCreated, Source: "user",
			Payload: map[string]any{"teamId": teamA, "mode": team.ModeSingle, "autoPlan": true, "planHash": "hand-p3r-a", "taskCount": 2}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runA, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "root", "memberId": "lead"}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runA, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": longSource, "memberId": "lead"}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runA, Type: team.EventTaskCompleted, Source: "watcher",
			Payload: map[string]any{"taskId": "root", "sessionId": sessionA, "turnId": "turn-p3r", "result": "主线产出", "resultChars": 4}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runA, Type: team.EventTaskCompleted, Source: "watcher",
			Payload: map[string]any{"taskId": longSource, "sessionId": sessionA, "turnId": "turn-p3r", "result": "主线产出", "resultChars": 4}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runB, Type: team.EventRunCreated, Source: "user",
			Payload: map[string]any{"teamId": teamB, "mode": team.ModeSingle, "autoPlan": true, "planHash": "hand-p3r-b", "taskCount": 2}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runB, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "root", "memberId": "lead"}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runB, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "b-src", "memberId": "lead"}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runB, Type: team.EventTaskCompleted, Source: "watcher",
			Payload: map[string]any{"taskId": "root", "sessionId": sessionLeadB, "turnId": "turn-p3r", "result": "主线产出", "resultChars": 4}, CreatedAt: now},
		{ID: uuid.NewString(), RunID: runB, Type: team.EventTaskCompleted, Source: "watcher",
			Payload: map[string]any{"taskId": "b-src", "sessionId": sessionLeadB, "turnId": "turn-p3r", "result": "产出", "resultChars": 2}, CreatedAt: now},
	}
	// 预设动作的 action_proposed 事实（journal 为动作执行/重建的权威源）。
	appendProposed := func(runID, teamID string, action team.Action) {
		events = append(events, &team.Event{ID: uuid.NewString(), RunID: runID, Type: team.EventActionProposed,
			Source: "watcher", IdempotencyKey: action.ActionKey, CreatedAt: now,
			Payload: map[string]any{"taskId": action.SourceTaskID, "actionKey": action.ActionKey,
				"type": action.Type, "round": action.Round, "sourceTaskId": action.SourceTaskID,
				"member": action.Member, "taskHead": action.Task}})
	}
	for _, action := range runARow.PendingActions {
		appendProposed(runA, teamA, action)
	}
	for _, action := range runBRow.PendingActions {
		appendProposed(runB, teamB, action)
	}
	writeTeamPhase3Files(t, dataDir, teams,
		map[string]*team.Run{runA: runARow, runB: runBRow}, events, nil,
		sessionA+"\x00"+dataDir+"\n"+sessionDevA+"\x00"+dataDir+"\n"+sessionLeadB+"\x00"+dataDir+"\n")
	return teamA, runA, teamB, runB, sessionA
}

// TestTeamPhase3ActionRejectionsExplained：执行侧拒绝各有可解释中文原因——
// action_rejected 事件 + 快照 pendingActions[].status=rejected/reason 非空
// （评审硬伤②的前端数据源）；autoDispatch=false 的动作停在 proposed（预览）
// 且不阻塞 Run 终局。
func TestTeamPhase3ActionRejectionsExplained(t *testing.T) {
	dataDir := t.TempDir()
	teamA, runA, teamB, runB, _ := writeTeamPhase3RejectState(t, dataDir)
	env := newTeamIntegrationEnv(t, dataDir)
	if _, err := env.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	if err := env.server.TeamFeeds.PrimeFromJournal(); err != nil {
		t.Fatalf("PrimeFromJournal: %v", err)
	}
	if err := env.server.Teams.RecoverAll(); err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamA, runA)["status"] == "completed" &&
			env.runDetail(t, teamB, runB)["status"] == "completed"
	}) {
		t.Fatal("两条 Run 未在时限内收口")
	}

	reasons := map[string]string{}
	detailA := env.runDetail(t, teamA, runA)
	for _, action := range phase3Actions(t, detailA) {
		if action["status"] != "rejected" {
			t.Fatalf("团队 A 的动作应全部 rejected: %v", action)
		}
		reason, _ := action["reason"].(string)
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("拒绝动作的 reason 不得为空（前端数据源）: %v", action)
		}
		key, _ := action["actionKey"].(string)
		reasons[key] = reason
	}
	wantReasons := []struct{ key, needle string }{
		{team.ActionKey(teamA, runA, "root", 1, 0), "引用了不存在的成员：ghost"},
		{team.ActionKey(teamA, runA, "root", 9, 1), "实际 9"},
		{team.ActionKey(teamA, runA, "s"+strings.Repeat("y", 61), 1, 2), "派生任务 ID 超长"},
		{team.ActionKey(teamA, runA, "root", 1, 3), "任务数已达上限 2"},
	}
	for _, want := range wantReasons {
		reason, ok := reasons[want.key]
		if !ok || !strings.Contains(reason, want.needle) {
			t.Fatalf("动作 %s 的拒绝原因应含 %q: %q（全集 %v）", want.key, want.needle, reason, reasons)
		}
	}
	events := env.journalEvents(t)
	if count := runEventCount(events, runA, team.EventActionRejected); count != 4 {
		t.Fatalf("应恰 4 条 action_rejected: %d", count)
	}
	if got := len(detailA["tasks"].([]any)); got != 2 {
		t.Fatalf("被拒动作不得创建派生任务: %d", got)
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 0 {
		t.Fatalf("全程零派发: %v", records)
	}

	// autoDispatch=false：动作预览（停 proposed、无拒绝事件、不阻塞终局）。
	detailB := env.runDetail(t, teamB, runB)
	actionsB := phase3Actions(t, detailB)
	if len(actionsB) != 1 {
		t.Fatalf("预览动作应保留在快照: %v", actionsB)
	}
	if actionsB[0]["status"] != "proposed" || actionsB[0]["reason"] != nil {
		t.Fatalf("闸门关闭的动作应停在 proposed 且无 reason: %v", actionsB[0])
	}
	if count := runEventCount(events, runB, team.EventActionRejected); count != 0 {
		t.Fatalf("预览动作不应产生 action_rejected: %d", count)
	}
}

// ---- 用例 4：§13.2:14 escalate——提问登记、回答、幂等、超时豁免 ----

// TestTeamPhase3EscalateQuestionLifecycle：成员在任务 turn 中发 AskUserQuestion
// 提问卡 → watcher 登记 pendingQuestions（questionId/version/options）→ GET
// questions 一致 → 选项无效 400 保持 open → 版本过期 409 → 开放问题期间任务
// 超时不触发（taskTimeoutSecs=1 跨两个超时窗口仍 running）→ 有效回答送达伪
// Agent（answers.log）、turn 收尾、问题 answered → 同 decisionID 重放
// duplicate 且不再投递 → 异 decisionID 二次回答 409。
func TestTeamPhase3EscalateQuestionLifecycle(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionLead := env.createMemberSession(t, "lead-sess", "提问团队 Lead")
	sessionDev := env.createMemberSession(t, "dev-sess", "提问成员")
	response := env.request(http.MethodPost, "/api/teams", `{"name":"阶段三提问团队","policy":{"autoDispatch":true,"taskTimeoutSecs":1},
		"members":[{"id":"lead","sessionId":"`+sessionLead+`","role":"lead"},
		{"id":"dev","sessionId":"`+sessionDev+`","role":"member"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	teamID := created.ID
	runID := env.createRun(t, teamID, `{"goal":"提问验收","mode":"pipeline","plan":[
		{"id":"t1","memberId":"dev","prompt":"提问任务 PH3-ASKQ ASKQ 请选择部署环境","writeLock":"none"}]}`)

	// 登记面：detail.pendingQuestions 出现（questionId/version/options）。
	var question map[string]any
	if !env.drive(t, 20*time.Second, func() bool {
		detail := env.runDetail(t, teamID, runID)
		questions := phase3Questions(t, detail)
		if len(questions) != 1 {
			return false
		}
		question = questions[0]
		return question["status"] == "open"
	}) {
		t.Fatal("提问卡未在时限内登记为 pendingQuestion")
	}
	questionID, _ := question["questionId"].(string)
	if questionID != "t1:ask-1" {
		t.Fatalf("questionId 应为 {taskID}:{requestID}: %q", questionID)
	}
	if question["kind"] != "question" || !strings.Contains(question["summary"].(string), "部署环境") {
		t.Fatalf("问题 kind/summary 异常: %v", question)
	}
	options, _ := question["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("应携带两个选项: %v", options)
	}
	registeredVersion := uint64(question["version"].(float64))

	// GET questions 与 detail 一致。
	questionsResponse := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/questions", "")
	if questionsResponse.Code != http.StatusOK {
		t.Fatalf("questions 应 200: %d %s", questionsResponse.Code, questionsResponse.Body.String())
	}
	var questionsPage struct {
		Questions []map[string]any `json:"questions"`
	}
	if err := json.Unmarshal(questionsResponse.Body.Bytes(), &questionsPage); err != nil {
		t.Fatal(err)
	}
	if len(questionsPage.Questions) != 1 || questionsPage.Questions[0]["questionId"] != questionID ||
		questionsPage.Questions[0]["status"] != "open" {
		t.Fatalf("GET questions 应与 detail 一致: %s", questionsResponse.Body.String())
	}

	// 选项无效：400 且问题保持 open 可重答。
	invalid := env.decide(t, teamID, runID, uuid.NewString(), questionID, registeredVersion, `{"optionId":"nope"}`)
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "请选择有效的选项") {
		t.Fatalf("选项无效应 400: %d %s", invalid.Code, invalid.Body.String())
	}
	if q := phase3FindQuestion(t, env.runDetail(t, teamID, runID), questionID); q == nil || q["status"] != "open" {
		t.Fatalf("无效回答不得消费问题: %v", q)
	}
	// 版本过期：409 且要求刷新。
	stale := env.decide(t, teamID, runID, uuid.NewString(), questionID, registeredVersion+1, `{"optionId":"opt-a"}`)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "已过期") {
		t.Fatalf("过期版本应 409: %d %s", stale.Code, stale.Body.String())
	}
	if q := phase3FindQuestion(t, env.runDetail(t, teamID, runID), questionID); q == nil || q["status"] != "open" {
		t.Fatalf("过期回答不得消费问题: %v", q)
	}

	// 开放问题期间任务超时不触发：跨两个超时窗口（taskTimeoutSecs=1）任务
	// 仍 running、Run 不失败（超时豁免把 dispatchedAt 重臂，用户未回答不烧
	// 时钟——豁免状态在 runner 内存，可观测面是任务跨窗存活）。
	questionOpenSince := time.Now()
	if !awaitCond(t, 5*time.Second, func() bool {
		return time.Since(questionOpenSince) > 2600*time.Millisecond
	}) {
		t.Fatal("等待超时窗口跨线失败")
	}
	detail := env.runDetail(t, teamID, runID)
	if task := teamTaskOf(t, detail, "t1"); task["status"] != "running" {
		t.Fatalf("开放问题期间任务不得超时: %v", task["status"])
	}
	if status := detail["status"]; status == "failed" || status == "cancelled" {
		t.Fatalf("开放问题期间 Run 不得失败/取消: %v", status)
	}

	// 有效回答：送达伪 Agent（respond 答案落 answers.log）、turn 收尾、answered。
	decisionID := uuid.NewString()
	valid := env.decide(t, teamID, runID, decisionID, questionID, registeredVersion, `{"optionId":"opt-a"}`)
	var validBody struct {
		OK        bool `json:"ok"`
		Duplicate bool `json:"duplicate"`
	}
	if valid.Code != http.StatusOK || json.Unmarshal(valid.Body.Bytes(), &validBody) != nil || !validBody.OK || validBody.Duplicate {
		t.Fatalf("有效回答应 200 {ok:true}: %d %s", valid.Code, valid.Body.String())
	}
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("回答后 Run 未在时限内完成")
	}
	answers := readAgentAnswers(t, env.dataDir)
	outcome, ok := answers["ask-1"]
	if !ok || !strings.Contains(outcome, "opt-a") || !strings.Contains(outcome, "selected") {
		t.Fatalf("伪 Agent 应收到 selected/opt-a 的 respond: %v", answers)
	}
	if q := phase3FindQuestion(t, env.runDetail(t, teamID, runID), questionID); q == nil || q["status"] != "answered" {
		t.Fatalf("问题应 answered: %v", q)
	}

	// 同 decisionID 重放：duplicate=true 且不再投递（answers.log 恰一条）。
	replay := env.decide(t, teamID, runID, decisionID, questionID, registeredVersion, `{"optionId":"opt-a"}`)
	var replayBody struct {
		OK        bool `json:"ok"`
		Duplicate bool `json:"duplicate"`
	}
	if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &replayBody) != nil ||
		!replayBody.OK || !replayBody.Duplicate {
		t.Fatalf("同 decisionID 重放应 duplicate:true: %d %s", replay.Code, replay.Body.String())
	}
	if answers = readAgentAnswers(t, env.dataDir); len(answers) != 1 {
		t.Fatalf("重放不得二次投递: %v", answers)
	}
	// 异 decisionID 二次回答：409（答案只允许消费一次）。
	second := env.decide(t, teamID, runID, uuid.NewString(), questionID, registeredVersion, `{"optionId":"opt-a"}`)
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "已回答或已撤回") {
		t.Fatalf("二次回答应 409: %d %s", second.Code, second.Body.String())
	}
	// 事件面：opened/resolved 各一，无 withdrawn（重复提交只产生一个恢复动作）。
	events := env.journalEvents(t)
	if count := teamEventCount(events, team.EventQuestionOpened); count != 1 {
		t.Fatalf("question_opened 应恰一次: %d", count)
	}
	if count := teamEventCount(events, team.EventQuestionResolved); count != 1 {
		t.Fatalf("question_resolved 应恰一次（重复回答不产生第二个恢复动作）: %d", count)
	}
	if count := teamEventCount(events, team.EventQuestionWithdrawn); count != 0 {
		t.Fatalf("全程不应有 question_withdrawn: %d", count)
	}
}

// TestTeamPhase3QuestionCapAndCancelWithdraw：同 Task 未决问题上限 3（第 4 张
// 卡不登记）；回答释放额度后第 4 张卡补登记；取消 Run 后未决问题全部
// withdrawn、再答 409。
func TestTeamPhase3QuestionCapAndCancelWithdraw(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionLead := env.createMemberSession(t, "lead-sess", "上限团队 Lead")
	sessionDev := env.createMemberSession(t, "dev-sess", "上限提问成员")
	response := env.request(http.MethodPost, "/api/teams", `{"name":"阶段三上限团队",
		"members":[{"id":"lead","sessionId":"`+sessionLead+`","role":"lead"},
		{"id":"dev","sessionId":"`+sessionDev+`","role":"member"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	teamID := created.ID
	runID := env.createRun(t, teamID, `{"goal":"上限验收","mode":"pipeline","plan":[
		{"id":"t1","memberId":"dev","prompt":"多问题任务 PH3-ASKQ4 ASKQ4 请逐一确认","writeLock":"none"}]}`)

	// 4 张卡同帧在途：恰登记 3 张（ask-1..3），ask-4 被 3 上限挡住。
	if !env.drive(t, 20*time.Second, func() bool {
		return phase3OpenQuestionCount(t, env.runDetail(t, teamID, runID), "t1") == 3
	}) {
		t.Fatal("未决问题上限应为 3")
	}
	detail := env.runDetail(t, teamID, runID)
	for _, id := range []string{"t1:ask-1", "t1:ask-2", "t1:ask-3"} {
		if q := phase3FindQuestion(t, detail, id); q == nil || q["status"] != "open" {
			t.Fatalf("问题 %s 应登记为 open: %v", id, q)
		}
	}
	if q := phase3FindQuestion(t, detail, "t1:ask-4"); q != nil {
		t.Fatalf("第 4 张卡不得越过上限登记: %v", q)
	}
	// 稳定窗：watcher 持续重试 ask-4 也始终 3 张。
	env.drive(t, 800*time.Millisecond, func() bool { return false })
	if count := phase3OpenQuestionCount(t, env.runDetail(t, teamID, runID), "t1"); count != 3 {
		t.Fatalf("上限应稳定在 3: %d", count)
	}

	// 回答释放额度：ask-1 answered 后 ask-4 补登记（额度复用）。
	var ask1 map[string]any
	for _, question := range phase3Questions(t, env.runDetail(t, teamID, runID)) {
		if question["questionId"] == "t1:ask-1" {
			ask1 = question
		}
	}
	if ask1 == nil {
		t.Fatal("未取到 ask-1")
	}
	if decide := env.decide(t, teamID, runID, uuid.NewString(), "t1:ask-1",
		uint64(ask1["version"].(float64)), `{"optionId":"opt-a"}`); decide.Code != http.StatusOK {
		t.Fatalf("回答 ask-1 失败: %d %s", decide.Code, decide.Body.String())
	}
	if !env.drive(t, 20*time.Second, func() bool {
		return phase3FindQuestion(t, env.runDetail(t, teamID, runID), "t1:ask-4") != nil
	}) {
		t.Fatal("额度释放后第 4 张卡应补登记")
	}

	// 取消 Run：未决问题全部 withdrawn，再答 409。
	if cancel := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`); cancel.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", cancel.Code, cancel.Body.String())
	}
	if !env.drive(t, 20*time.Second, func() bool {
		detail := env.runDetail(t, teamID, runID)
		for _, question := range phase3Questions(t, detail) {
			if question["status"] == "open" {
				return false
			}
		}
		return detail["status"] == "cancelled"
	}) {
		t.Fatal("取消后未决问题应全部撤回")
	}
	detail = env.runDetail(t, teamID, runID)
	var ask2 map[string]any
	for _, question := range phase3Questions(t, detail) {
		if question["questionId"] == "t1:ask-2" {
			ask2 = question
		}
	}
	if ask2 == nil || ask2["status"] != "withdrawn" {
		t.Fatalf("ask-2 应 withdrawn: %v", ask2)
	}
	if count := teamEventCount(env.journalEvents(t), team.EventQuestionWithdrawn); count < 3 {
		t.Fatalf("撤回事件应覆盖全部未决问题: %d", count)
	}
	withdrawn := env.decide(t, teamID, runID, uuid.NewString(), "t1:ask-2",
		uint64(ask2["version"].(float64)), `{"optionId":"opt-a"}`)
	if withdrawn.Code != http.StatusConflict || !strings.Contains(withdrawn.Body.String(), "已回答或已撤回") {
		t.Fatalf("撤回后再答应 409: %d %s", withdrawn.Code, withdrawn.Body.String())
	}
}

// ---- 用例 5：§13.2:13 amend——inputHash 前缀复用与断链 ----

// TestTeamPhase3AmendCacheImport：3 任务 pipeline 完成后修订末任务——前 2
// 任务 cache_hit 直落 completed（零重发、prompts.log 无新条目）、末任务真
// 执行、事件含 task_cache_hit×2、父 Run journal 零增、parentRunId/
// cacheImports 投影正确、同 id 重放 duplicate、同 id 异内容 400；变体：头
// 任务异哈希断链（零导入）、依赖变化断链（前缀截断在变化处）、插入新
// exclusive 任务前缀清零。
func TestTeamPhase3AmendCacheImport(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionLead := env.createMemberSession(t, "lead-sess", "amend Lead")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionLead+`","role":"lead"}]`)
	const planBody = `{"id":"t1","memberId":"lead","prompt":"第一阶段产出 PH3-AM1","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"第二阶段产出 PH3-AM2","writeLock":"none","dependsOn":["t1"]},
		{"id":"t3","memberId":"lead","prompt":"第三阶段产出 PH3-AM3","writeLock":"none","dependsOn":["t2"]}`
	runID := env.createRun(t, teamID, `{"goal":"amend 基线","mode":"pipeline","plan":[`+planBody+`]}`)
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("基线 Run 未在时限内完成")
	}
	parentDetail := env.runDetail(t, teamID, runID)
	parentT1 := taskResultOf(t, teamTaskOf(t, parentDetail, "t1"))
	parentEventsBefore := len(env.journalEvents(t))
	promptsBefore := len(readTeamPrompts(t, env.dataDir))

	amendID := uuid.NewString()
	page, code, bodyText := env.amend(t, teamID, runID, `{"id":"`+amendID+`","goal":"amend 修订","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"第一阶段产出 PH3-AM1","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"第二阶段产出 PH3-AM2","writeLock":"none","dependsOn":["t1"]},
		{"id":"t3","memberId":"lead","prompt":"第三阶段修订产出 PH3-AM3B","writeLock":"none","dependsOn":["t2"]}]}`)
	if code != http.StatusOK {
		t.Fatalf("amend 创建失败: %d %s", code, bodyText)
	}
	if page["id"] != amendID || page["parentRunId"] != runID {
		t.Fatalf("amend Run 应携带自身 id 与 parentRunId: %v %v", page["id"], page["parentRunId"])
	}
	imports, _ := page["cacheImports"].([]any)
	if len(imports) != 2 {
		t.Fatalf("前缀应命中 2 个缓存导入: %v", imports)
	}
	for _, row := range imports {
		entry, _ := row.(map[string]any)
		taskID, _ := entry["taskId"].(string)
		if taskID != "t1" && taskID != "t2" {
			t.Fatalf("缓存导入应只含 t1/t2: %v", entry)
		}
		if entry["parentTaskId"] != taskID || entry["inputHash"] == "" ||
			entry["sessionId"] != sessionLead || entry["turnId"] == "" {
			t.Fatalf("缓存导入应携带溯源引用: %v", entry)
		}
	}
	// 新 Run 起跑即含两份缓存完成事实（§6.4.1 规则 4：事件一个不少）。
	events := env.journalEvents(t)
	if count := runEventCount(events, amendID, team.EventTaskCacheHit); count != 2 {
		t.Fatalf("应恰 2 条 task_cache_hit: %d", count)
	}
	// §13.3:9 cache_hit 可见：changes reset 页含 task_cache_hit 行。
	changes := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+amendID+"/changes?after=-1", "")
	if changes.Code != http.StatusOK || !strings.Contains(changes.Body.String(), "task_cache_hit") {
		t.Fatalf("changes 应携带 task_cache_hit 行: %d %s", changes.Code, changes.Body.String())
	}

	// 修订 Run 完成：仅末任务真执行（prompts 恰 +1），前缀零重发。
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, amendID)["status"] == "completed"
	}) {
		t.Fatal("amend Run 未在时限内完成")
	}
	records := readTeamPrompts(t, env.dataDir)
	if len(records) != promptsBefore+1 {
		t.Fatalf("前缀缓存零重发、末任务恰执行一次: %d ≠ %d", len(records), promptsBefore+1)
	}
	amendAt := promptIndex(records, "PH3-AM3B")
	if amendAt < 0 || records[amendAt].session != sessionLead {
		t.Fatalf("修订末任务的提示词应到达 Lead 会话: %v", records)
	}
	// 增量恰一条且是修订末任务（上下文携带 t1/t2 结果属正常注入——缓存
	// 零重发由「增量恰 1 条」与下方 cachedFrom 断言共同保证）。
	amendDetail := env.runDetail(t, teamID, amendID)
	parentRefs := map[string]map[string]any{
		"t1": parentT1,
		"t2": taskResultOf(t, teamTaskOf(t, parentDetail, "t2")),
	}
	for _, taskID := range []string{"t1", "t2"} {
		task := teamTaskOf(t, amendDetail, taskID)
		if task["status"] != "completed" || task["cachedFrom"] != taskID {
			t.Fatalf("任务 %s 应缓存完成并携带 cachedFrom: %v", taskID, task)
		}
		if result := taskResultOf(t, task); result["sessionId"] != parentRefs[taskID]["sessionId"] ||
			result["turnId"] != parentRefs[taskID]["turnId"] {
			t.Fatalf("缓存结果应保留父 Run 的结果引用: %v vs %v", result, parentRefs[taskID])
		}
	}
	if task := teamTaskOf(t, amendDetail, "t3"); task["cachedFrom"] != nil || task["status"] != "completed" {
		t.Fatalf("修订末任务应真执行（无 cachedFrom）: %v", task)
	}
	// 父 Run 不可变：journal 事件零增、父状态不变。
	events = env.journalEvents(t)
	parentEventsAfter := 0
	for _, event := range events {
		if event.RunID == runID {
			parentEventsAfter++
		}
	}
	if parentEventsBefore != parentEventsAfter {
		t.Fatalf("amend 不得写父 Run 的 journal: %d ≠ %d", parentEventsBefore, parentEventsAfter)
	}
	if parent := env.runDetail(t, teamID, runID); parent["status"] != "completed" {
		t.Fatalf("父 Run 状态不得改变: %v", parent["status"])
	}

	// 同 id 同内容重放：duplicate:true、不建新 Run。
	replayPage, replayCode, _ := env.amend(t, teamID, runID, `{"id":"`+amendID+`","goal":"amend 修订","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"第一阶段产出 PH3-AM1","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"第二阶段产出 PH3-AM2","writeLock":"none","dependsOn":["t1"]},
		{"id":"t3","memberId":"lead","prompt":"第三阶段修订产出 PH3-AM3B","writeLock":"none","dependsOn":["t2"]}]}`)
	if replayCode != http.StatusOK || replayPage["duplicate"] != true || replayPage["id"] != amendID {
		t.Fatalf("同 id 重放应 duplicate:true: %d %v", replayCode, replayPage)
	}
	list := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs", "")
	var runList struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &runList); err != nil {
		t.Fatal(err)
	}
	if len(runList.Runs) != 2 {
		t.Fatalf("重放不得创建新 Run: %d", len(runList.Runs))
	}
	// 同 id 异内容：400。
	_, mutatedCode, mutatedBody := env.amend(t, teamID, runID, `{"id":"`+amendID+`","goal":"amend 修订","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"第一阶段产出 PH3-AM1","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"第二阶段产出 PH3-AM2","writeLock":"none","dependsOn":["t1"]},
		{"id":"t3","memberId":"lead","prompt":"第三阶段另一种产出 PH3-AM3C","writeLock":"none","dependsOn":["t2"]}]}`)
	if mutatedCode != http.StatusBadRequest || !strings.Contains(mutatedBody, "同一创建请求不能用于不同内容") {
		t.Fatalf("同 id 异内容应 400: %d %s", mutatedCode, mutatedBody)
	}

	// 变体（断言 cacheImports 后即取消，不驱动执行）。
	cancelAmend := func(id string) {
		t.Helper()
		if cancel := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+id+"/cancel", `{}`); cancel.Code != http.StatusOK {
			t.Fatalf("清理取消失败: %d %s", cancel.Code, cancel.Body.String())
		}
	}
	// 头任务异哈希：前缀在首个任务处断链，零导入（t1 修订后 t2/t3 也无从复用）。
	headChanged, headCode, headBody := env.amend(t, teamID, runID, `{"id":"`+uuid.NewString()+`","goal":"amend 头修订","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"第一阶段修订产出 PH3-AM1B","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"第二阶段产出 PH3-AM2","writeLock":"none","dependsOn":["t1"]},
		{"id":"t3","memberId":"lead","prompt":"第三阶段产出 PH3-AM3","writeLock":"none","dependsOn":["t2"]}]}`)
	if headCode != http.StatusOK {
		t.Fatalf("头修订 amend 创建失败: %d %s", headCode, headBody)
	}
	if imports, _ = headChanged["cacheImports"].([]any); len(imports) != 0 {
		t.Fatalf("头任务异哈希应零导入: %v", imports)
	}
	cancelAmend(headChanged["id"].(string))
	// 依赖变化断链：t3 的 dependsOn 变化 → 自身 inputHash 变化，前缀截断在 t3。
	depChanged, depCode, depBody := env.amend(t, teamID, runID, `{"id":"`+uuid.NewString()+`","goal":"amend 依赖修订","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"第一阶段产出 PH3-AM1","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"第二阶段产出 PH3-AM2","writeLock":"none","dependsOn":["t1"]},
		{"id":"t3","memberId":"lead","prompt":"第三阶段产出 PH3-AM3","writeLock":"none","dependsOn":["t1","t2"]}]}`)
	if depCode != http.StatusOK {
		t.Fatalf("依赖修订 amend 创建失败: %d %s", depCode, depBody)
	}
	if imports, _ = depChanged["cacheImports"].([]any); len(imports) != 2 {
		t.Fatalf("依赖变化应断链在 t3（导入恰 t1/t2）: %v", imports)
	}
	for _, row := range imports {
		if entry, _ := row.(map[string]any); entry["taskId"] == "t3" {
			t.Fatalf("依赖变化的 t3 不得导入: %v", entry)
		}
	}
	cancelAmend(depChanged["id"].(string))
	// 插入新 exclusive 任务：拓扑序首位无父任务对应，前缀整体清零。
	inserted, insertCode, insertBody := env.amend(t, teamID, runID, `{"id":"`+uuid.NewString()+`","goal":"amend 插入修订","mode":"pipeline","plan":[
		{"id":"t0","memberId":"lead","prompt":"插入的排他任务 PH3-AM0-NEW","writeLock":"exclusive"},
		{"id":"t1","memberId":"lead","prompt":"第一阶段产出 PH3-AM1","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"第二阶段产出 PH3-AM2","writeLock":"none","dependsOn":["t1"]},
		{"id":"t3","memberId":"lead","prompt":"第三阶段产出 PH3-AM3","writeLock":"none","dependsOn":["t2"]}]}`)
	if insertCode != http.StatusOK {
		t.Fatalf("插入修订 amend 创建失败: %d %s", insertCode, insertBody)
	}
	if imports, _ = inserted["cacheImports"].([]any); len(imports) != 0 {
		t.Fatalf("首位插入新任务应使前缀清零: %v", imports)
	}
	cancelAmend(inserted["id"].(string))
}

// ---- 用例 6：恢复面——proposed 动作 / pendingQuestions / 缓存任务 / journal 重建 ----

// writeTeamPhase3Files 落预设账本组合：teams.json / team-runs.json /
// team-journal.jsonl / submissions.json / sessions.txt（ nil 项跳过；journal
// 事件按切片序重编全局单调序号）。
func writeTeamPhase3Files(t *testing.T, dataDir string, teams map[string]*team.Team,
	runs map[string]*team.Run, events []*team.Event, ledger map[string]*submissionEntry, sessions string) {
	t.Helper()
	if teams != nil {
		raw, err := json.Marshal(teams)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "teams.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if runs != nil {
		raw, err := json.Marshal(runs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if events != nil {
		var journal strings.Builder
		for i, event := range events {
			event.Sequence = uint64(i + 1)
			line, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			journal.Write(line)
			journal.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dataDir, "team-journal.jsonl"), []byte(journal.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ledger != nil {
		raw, err := json.Marshal(ledger)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "submissions.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if sessions != "" {
		if err := os.WriteFile(filepath.Join(dataDir, "sessions.txt"), []byte(sessions), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeTeamPhase3RecoveryState 预置三条件恢复面（同一团队）：
//   - R-act：AutoPlan Run，快照带两条 proposed 动作（可执行的 delegate + 必拒
//     的未知成员 delegate），journal 已有对应 action_proposed——重启后 loop 补
//     执行，action_proposed 单 owner 不双写；
//   - R-q：在途任务（账本 accepted）+ 快照/journal 各带一条 open 问题——重启
//     后问题复原，decision 撞会话侧过期（卡片已不存在）触发 stale 撤回；
//   - R-cache：amend 子 Run，c1 缓存完成（CachedFrom）+ c2 planned——重启后
//     缓存任务保持 completed 不重发、c2 正常执行。
func writeTeamPhase3RecoveryState(t *testing.T, dataDir string) (teamID, runAct, runQ, runCache string) {
	t.Helper()
	now := time.Now()
	teamID, runAct, runQ, runCache = "tteam-p3v", "trun-p3v-act", "trun-p3v-q", "trun-p3v-cache"
	const sessionLead, sessionDev = "km-team-1", "km-team-2"
	policy := team.DefaultPolicy()
	policy.AutoDispatch = true

	leadResult := &team.TaskResult{SessionID: sessionLead, TurnID: "turn-p3v-act", Result: "主线产出", ResultChars: 4}
	runActRow := &team.Run{ID: runAct, TeamID: teamID, Goal: "动作恢复", Mode: team.ModeSingle,
		AutoPlan: true, Status: team.RunDispatching, Round: 1, PlanHash: "hand-p3v-act", Version: 1,
		Tasks: []team.Task{{ID: "lead", MemberID: "lead", Prompt: "主线任务", Ordinal: 0,
			Status: team.TaskCompleted, WriteLock: team.TaskWriteLockNone, Result: "主线产出", ResultDetail: leadResult}},
		CreatedAt: now, UpdatedAt: now}
	actionOK := team.Action{Type: team.ActionTypeDelegate, SourceTaskID: "lead", Round: 2, Member: "dev",
		Task: "恢复补执行任务 PH3-RECOVER", ActionKey: team.ActionKey(teamID, runAct, "lead", 2, 0), Status: team.ActionStatusProposed}
	actionBad := team.Action{Type: team.ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "ghost",
		Task: "未知成员动作", ActionKey: team.ActionKey(teamID, runAct, "lead", 1, 1), Status: team.ActionStatusProposed}
	runActRow.PendingActions = []team.Action{actionOK, actionBad}

	qPrompt := "在途提问任务 PH3-RECQ"
	qTask := &team.Task{ID: "q1", MemberID: "dev", Prompt: qPrompt, Attempt: 1, Ordinal: 0,
		Status: team.TaskRunning, WriteLock: team.TaskWriteLockNone}
	qTask.InputHash = team.InputHashFor(qTask, team.PolicyDigest(policy), "")
	qTask.SentText = qPrompt
	qTask.SubmissionID = team.TaskSubmissionID(teamID, runQ, "q1", 1)
	runQRow := &team.Run{ID: runQ, TeamID: teamID, Goal: "问题恢复", Mode: team.ModePipeline,
		Status: team.RunDispatching, Round: 1, PlanHash: "hand-p3v-q", Version: 3,
		Tasks: []team.Task{*qTask}, CreatedAt: now, UpdatedAt: now}
	runQRow.PendingQuestions = []team.PendingQuestion{{
		QuestionID: "q1:ask-9", TaskID: "q1", Version: 3, Kind: team.QuestionKindQuestion,
		Summary: "恢复面问题：部署环境选择", Status: team.QuestionStatusOpen, OpenedAt: now,
		Options: []team.QuestionOption{{ID: "opt-a", Label: "继续"}, {ID: "opt-b", Label: "跳过"}},
	}}

	cacheResult := &team.TaskResult{SessionID: sessionLead, TurnID: "turn-p3v-cache", Result: "缓存产出", ResultChars: 4}
	runCacheRow := &team.Run{ID: runCache, TeamID: teamID, Goal: "缓存恢复", Mode: team.ModePipeline,
		ParentRunID: "trun-p3v-parent", Status: team.RunDispatching, Round: 1, PlanHash: "hand-p3v-cache", Version: 1,
		Tasks: []team.Task{
			{ID: "c1", MemberID: "lead", Prompt: "缓存任务原文", Ordinal: 0, Status: team.TaskCompleted,
				WriteLock: team.TaskWriteLockNone, Result: "缓存产出", ResultDetail: cacheResult, CachedFrom: "t1"},
			{ID: "c2", MemberID: "dev", Prompt: "恢复执行新任务 PH3-CACHE-NEW", Ordinal: 1,
				Status: team.TaskPlanned, WriteLock: team.TaskWriteLockNone},
		}, CreatedAt: now, UpdatedAt: now}

	teams := map[string]*team.Team{teamID: {ID: teamID, Name: "恢复面团队",
		Members: []team.TeamMember{
			{ID: "lead", SessionID: sessionLead, AgentKind: "kimi", Role: team.RoleLead, Enabled: true},
			{ID: "dev", SessionID: sessionDev, AgentKind: "kimi", Role: team.RoleMember, Enabled: true},
		}, LeadID: "lead", Policy: policy, CreatedAt: now, UpdatedAt: now}}
	events := []*team.Event{
		{ID: uuid.NewString(), RunID: runAct, Type: team.EventRunCreated, Source: "user", CreatedAt: now,
			Payload: map[string]any{"teamId": teamID, "mode": team.ModeSingle, "autoPlan": true, "planHash": "hand-p3v-act", "taskCount": 1}},
		{ID: uuid.NewString(), RunID: runAct, Type: team.EventTaskPlanned, Source: "user", CreatedAt: now,
			Payload: map[string]any{"taskId": "lead", "memberId": "lead"}},
		{ID: uuid.NewString(), RunID: runAct, Type: team.EventTaskCompleted, Source: "watcher", CreatedAt: now,
			Payload: map[string]any{"taskId": "lead", "sessionId": sessionLead, "turnId": "turn-p3v-act", "result": "主线产出", "resultChars": 4}},
		{ID: uuid.NewString(), RunID: runAct, Type: team.EventActionProposed, Source: "watcher", CreatedAt: now,
			IdempotencyKey: actionOK.ActionKey,
			Payload: map[string]any{"taskId": "lead", "actionKey": actionOK.ActionKey, "type": actionOK.Type,
				"round": 2, "sourceTaskId": "lead", "member": "dev", "taskHead": actionOK.Task}},
		{ID: uuid.NewString(), RunID: runAct, Type: team.EventActionProposed, Source: "watcher", CreatedAt: now,
			IdempotencyKey: actionBad.ActionKey,
			Payload: map[string]any{"taskId": "lead", "actionKey": actionBad.ActionKey, "type": actionBad.Type,
				"round": 1, "sourceTaskId": "lead", "member": "ghost", "taskHead": actionBad.Task}},
		{ID: uuid.NewString(), RunID: runQ, Type: team.EventRunCreated, Source: "user", CreatedAt: now,
			Payload: map[string]any{"teamId": teamID, "mode": team.ModePipeline, "planHash": "hand-p3v-q", "taskCount": 1}},
		{ID: uuid.NewString(), RunID: runQ, Type: team.EventTaskPlanned, Source: "user", CreatedAt: now,
			Payload: map[string]any{"taskId": "q1", "memberId": "dev"}},
		{ID: uuid.NewString(), RunID: runQ, Type: team.EventTaskDispatching, Source: "runner", CreatedAt: now,
			Payload: map[string]any{"taskId": "q1", "sessionId": sessionDev, "submissionId": qTask.SubmissionID,
				"inputHash": qTask.InputHash, "attempt": 1}},
		{ID: uuid.NewString(), RunID: runQ, Type: team.EventQuestionOpened, Source: "watcher", CreatedAt: now,
			Payload: map[string]any{"questionId": "q1:ask-9", "taskId": "q1", "version": 3,
				"kind": team.QuestionKindQuestion, "summary": "恢复面问题：部署环境选择",
				"options": []any{map[string]any{"id": "opt-a", "label": "继续"}, map[string]any{"id": "opt-b", "label": "跳过"}}}},
		{ID: uuid.NewString(), RunID: runCache, Type: team.EventRunCreated, Source: "user", CreatedAt: now,
			Payload: map[string]any{"teamId": teamID, "mode": team.ModePipeline, "planHash": "hand-p3v-cache",
				"taskCount": 2, "parentRunId": "trun-p3v-parent"}},
		{ID: uuid.NewString(), RunID: runCache, Type: team.EventTaskPlanned, Source: "user", CreatedAt: now,
			Payload: map[string]any{"taskId": "c1", "memberId": "lead"}},
		{ID: uuid.NewString(), RunID: runCache, Type: team.EventTaskCacheHit, Source: "user", CreatedAt: now,
			Payload: map[string]any{"taskId": "c1", "parentRunId": "trun-p3v-parent", "parentTaskId": "t1",
				"sessionId": sessionLead, "turnId": "turn-p3v-cache", "result": "缓存产出", "resultChars": 4}},
		{ID: uuid.NewString(), RunID: runCache, Type: team.EventTaskPlanned, Source: "user", CreatedAt: now,
			Payload: map[string]any{"taskId": "c2", "memberId": "dev"}},
	}
	ledger := map[string]*submissionEntry{
		sessionDev + ":" + qTask.SubmissionID: {
			ID: qTask.SubmissionID, ThreadID: sessionDev, Host: "local",
			Text: qPrompt, Mode: "queue", Status: SubAccepted,
			CreatedAt: float64(time.Now().UnixMilli()) / 1000},
	}
	writeTeamPhase3Files(t, dataDir, teams,
		map[string]*team.Run{runAct: runActRow, runQ: runQRow, runCache: runCacheRow},
		events, ledger, sessionLead+"\x00"+dataDir+"\n"+sessionDev+"\x00"+dataDir+"\n")
	return teamID, runAct, runQ, runCache
}

// TestTeamPhase3RecoveryFaces：同 dataDir 重装配 + RecoverAll 后——proposed
// 动作被 loop 补执行（journal 同 ActionKey 仅一条 action_proposed、必拒动作
// 落 rejected）、pendingQuestions 复原且会话侧过期的问题被 stale 撤回、缓存
// 任务保持 completed 不重发；再把 team-runs.json 写坏按 journal 重建——
// pendingQuestions / pendingActions（含 rejected reason）/ 缓存完成 / Run.Round
// 全在。
func TestTeamPhase3RecoveryFaces(t *testing.T) {
	dataDir := t.TempDir()
	teamID, runAct, runQ, runCache := writeTeamPhase3RecoveryState(t, dataDir)
	env := newTeamIntegrationEnv(t, dataDir)
	if _, err := env.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	if err := env.server.TeamFeeds.PrimeFromJournal(); err != nil {
		t.Fatalf("PrimeFromJournal: %v", err)
	}
	if err := env.server.Teams.RecoverAll(); err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}

	// ① proposed 动作补执行：可执行 delegate 派生任务真跑完，未知成员被拒。
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, runAct)["status"] == "completed"
	}) {
		t.Fatal("动作恢复 Run 未在时限内补执行收口")
	}
	detailAct := env.runDetail(t, teamID, runAct)
	derived := teamTaskOf(t, detailAct, "lead-d2-0")
	if derived["status"] != "completed" || derived["result"] == nil {
		t.Fatalf("补执行的派生任务应完成: %v", derived)
	}
	if round := detailAct["round"]; round != float64(2) {
		t.Fatalf("round-2 动作执行后 Run.Round 应为 2: %v", round)
	}
	actions := phase3Actions(t, detailAct)
	if len(actions) != 2 || actions[0]["status"] != "executed" || actions[1]["status"] != "rejected" {
		t.Fatalf("动作应分别 executed/rejected: %v", actions)
	}
	if reason, _ := actions[1]["reason"].(string); !strings.Contains(reason, "引用了不存在的成员：ghost") {
		t.Fatalf("补执行期的拒绝应有可解释原因: %v", actions[1])
	}
	// 单 owner 不双写：同 ActionKey 的 action_proposed 恰一条。
	events := env.journalEvents(t)
	proposedOK := 0
	for _, event := range events {
		if event.Type == team.EventActionProposed && event.IdempotencyKey == team.ActionKey(teamID, runAct, "lead", 2, 0) {
			proposedOK++
		}
	}
	if proposedOK != 1 {
		t.Fatalf("action_proposed 单 owner 不双写: %d", proposedOK)
	}
	if count := runEventCount(events, runAct, team.EventActionRejected); count != 1 {
		t.Fatalf("应恰一条 action_rejected: %d", count)
	}
	// 派生任务恰派发一次（PH3-RECOVER 唯一到达）。
	if records := readTeamPrompts(t, dataDir); promptIndex(records, "PH3-RECOVER") < 0 {
		t.Fatalf("补执行任务应已派发: %v", records)
	}

	// ② pendingQuestions 复原 + 会话侧过期撤回：预设问题 open/version=3，
	// 成员会话是重启后的新进程（无 ask-9 卡）→ decision 走到 Respond 报过期
	// → 撤回 withdrawn(reason=stale) + 409。
	detailQ := env.runDetail(t, teamID, runQ)
	question := phase3FindQuestion(t, detailQ, "q1:ask-9")
	if question == nil || question["status"] != "open" || question["version"] != float64(3) {
		t.Fatalf("重启后问题应复原（open/version=3）: %v", question)
	}
	expired := env.decide(t, teamID, runQ, uuid.NewString(), "q1:ask-9", 3, `{"optionId":"opt-a"}`)
	if expired.Code != http.StatusConflict || !strings.Contains(expired.Body.String(), "请求已过期") {
		t.Fatalf("会话侧过期应 409: %d %s", expired.Code, expired.Body.String())
	}
	if !awaitCond(t, 20*time.Second, func() bool {
		question = phase3FindQuestion(t, env.runDetail(t, teamID, runQ), "q1:ask-9")
		return question != nil && question["status"] == "withdrawn"
	}) {
		t.Fatalf("过期问题应被 stale 撤回: %v", question)
	}
	// 收尾：取消在途 R-q（预设 turn 不存在，watcher 会一直等）。
	if cancel := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runQ+"/cancel", `{}`); cancel.Code != http.StatusOK {
		t.Fatalf("清理取消 R-q 失败: %d %s", cancel.Code, cancel.Body.String())
	}

	// ③ 缓存任务保持 completed 不重发，c2 正常执行。
	if !env.drive(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, runCache)["status"] == "completed"
	}) {
		t.Fatal("缓存恢复 Run 未在时限内收口")
	}
	detailCache := env.runDetail(t, teamID, runCache)
	c1 := teamTaskOf(t, detailCache, "c1")
	if c1["status"] != "completed" || c1["cachedFrom"] != "t1" || c1["result"] == nil {
		t.Fatalf("缓存任务应保持 completed 且携带来源: %v", c1)
	}
	if task := teamTaskOf(t, detailCache, "c2"); task["status"] != "completed" {
		t.Fatalf("c2 应补执行完成: %v", task)
	}
	records := readTeamPrompts(t, dataDir)
	if promptIndex(records, "缓存任务原文") >= 0 {
		t.Fatalf("缓存完成的任务不得重发: %v", records)
	}
	if promptIndex(records, "PH3-CACHE-NEW") < 0 {
		t.Fatalf("c2 应正常派发: %v", records)
	}

	// ④ team-runs.json 损坏 → journal 重建：问题/动作（含拒绝原因）/缓存完成/
	// Round 全在。
	if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	rebuilt := reopenTeamIntegrationEnv(t, dataDir)
	actView := rebuilt.runDetail(t, teamID, runAct)
	if round := actView["round"]; round != float64(2) {
		t.Fatalf("重建后 Run.Round 应由动作轮次增载到 2: %v", round)
	}
	rebuiltActions := phase3Actions(t, actView)
	if len(rebuiltActions) != 2 {
		t.Fatalf("重建后应恢复两条动作: %v", rebuiltActions)
	}
	for _, action := range rebuiltActions {
		key, _ := action["actionKey"].(string)
		if key == team.ActionKey(teamID, runAct, "lead", 1, 1) {
			if action["status"] != "rejected" || !strings.Contains(action["reason"].(string), "不存在的成员") {
				t.Fatalf("重建应恢复 rejected 动作及其原因: %v", action)
			}
		} else if action["status"] != "executed" {
			t.Fatalf("重建应按 task_planned(actionKey) 恢复 executed: %v", action)
		}
	}
	if derived := teamTaskOf(t, actView, "lead-d2-0"); derived["status"] != "completed" {
		t.Fatalf("重建应恢复派生任务完成事实: %v", derived)
	}
	qView := phase3FindQuestion(t, rebuilt.runDetail(t, teamID, runQ), "q1:ask-9")
	if qView == nil || qView["status"] != "withdrawn" || qView["version"] != float64(3) {
		t.Fatalf("重建应恢复 pendingQuestions（含撤回态与版本）: %v", qView)
	}
	cacheView := rebuilt.runDetail(t, teamID, runCache)
	if c1 := teamTaskOf(t, cacheView, "c1"); c1["status"] != "completed" || c1["cachedFrom"] != "t1" {
		t.Fatalf("重建应恢复缓存完成事实与来源: %v", c1)
	}
}

// ---- 用例 7：§13.3:9 input_hash_mismatch 可见 ----

// TestTeamPhase3InputHashMismatchVisible：快照 InputHash 与 journal 派发记录
// 漂移 → RecoverAll 落 task_input_hash_mismatch 事件、任务 stopped、Run
// waiting_input（§8.2 同一任务槽只接受一个输入哈希）。
func TestTeamPhase3InputHashMismatchVisible(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now()
	const teamID = "tteam-p3-hash"
	const runID = "trun-p3-hash"
	const sessionLead = "km-team-1"
	policy := team.DefaultPolicy()
	task := &team.Task{ID: "t1", MemberID: "lead", Prompt: "漂移任务 PH3-HASHDRIFT", Attempt: 1, Ordinal: 0,
		Status: team.TaskDispatching, WriteLock: team.TaskWriteLockNone}
	task.InputHash = "snapshot-hash-b"
	task.SubmissionID = team.TaskSubmissionID(teamID, runID, "t1", 1)
	task.SentText = task.Prompt
	run := &team.Run{ID: runID, TeamID: teamID, Goal: "哈希漂移", Mode: team.ModePipeline,
		Status: team.RunDispatching, Round: 1, PlanHash: "hand-p3-hash",
		Tasks: []team.Task{*task}, Version: 1, CreatedAt: now, UpdatedAt: now}
	writeTeamPhase3Files(t, dataDir,
		map[string]*team.Team{teamID: {ID: teamID, Name: "漂移团队",
			Members: []team.TeamMember{{ID: "lead", SessionID: sessionLead, AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
			LeadID:  "lead", Policy: policy, CreatedAt: now, UpdatedAt: now}},
		map[string]*team.Run{runID: run},
		[]*team.Event{
			{ID: uuid.NewString(), RunID: runID, Type: team.EventRunCreated, Source: "user", CreatedAt: now,
				Payload: map[string]any{"teamId": teamID, "mode": team.ModePipeline, "planHash": "hand-p3-hash", "taskCount": 1}},
			{ID: uuid.NewString(), RunID: runID, Type: team.EventTaskPlanned, Source: "user", CreatedAt: now,
				Payload: map[string]any{"taskId": "t1", "memberId": "lead"}},
			{ID: uuid.NewString(), RunID: runID, Type: team.EventTaskDispatching, Source: "runner", CreatedAt: now,
				Payload: map[string]any{"taskId": "t1", "sessionId": sessionLead, "submissionId": task.SubmissionID,
					"inputHash": "journal-hash-a", "attempt": 1}},
		}, nil, sessionLead+"\x00"+dataDir+"\n")

	env := reopenTeamIntegrationEnv(t, dataDir)
	if !awaitCond(t, 20*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("哈希漂移应把 Run 停进 waiting_input")
	}
	detail := env.runDetail(t, teamID, runID)
	if task := teamTaskOf(t, detail, "t1"); task["status"] != "stopped" {
		t.Fatalf("漂移任务应 stopped: %v", task["status"])
	}
	if reason, _ := detail["stopReason"].(string); !strings.Contains(reason, "输入哈希") {
		t.Fatalf("停靠原因应说明哈希不一致: %q", reason)
	}
	if count := teamEventCount(env.journalEvents(t), team.EventTaskInputHashMismatch); count != 1 {
		t.Fatalf("task_input_hash_mismatch 应恰一条: %d", count)
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 0 {
		t.Fatalf("漂移任务不得重发: %v", records)
	}
}
