package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/swarmeval"
	"github.com/google/uuid"
)

func swarmTestContract(goal string) SwarmContract {
	return SwarmContract{Criteria: []SwarmCriterion{{ID: "original", Text: goal, CheckKind: "human", Required: true}}, MaxRepairs: 2, MaxEvals: 12}
}

func swarmTestRun(t *testing.T, h *dispatchHarness, contract *SwarmContract, tasks []Task) *Run {
	t.Helper()
	run := &Run{ID: uuid.NewString(), TeamID: h.team.ID, Goal: "修复队列恢复", Status: RunDispatching, Tasks: tasks, Swarm: newSwarm("chain")}
	run.Swarm.Contract = contract
	if contract != nil {
		run.Swarm.ContractHash = goalHash(contract)
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestSwarmContractValidationRetainsOriginalAndKnownChecks(t *testing.T) {
	run := &Run{Goal: "原始目标"}
	valid := swarmTestContract(run.Goal)
	if err := validateSwarmContract(run, &valid); err != nil {
		t.Fatalf("纯咨询合法契约不能强加构建：%v", err)
	}
	for name, mutate := range map[string]func(*SwarmContract){
		"missing-original":  func(c *SwarmContract) { c.Criteria[0].ID = "other" },
		"reduced-original":  func(c *SwarmContract) { c.Criteria[0].Text = "调查如何修复" },
		"optional-original": func(c *SwarmContract) { c.Criteria[0].Required = false },
		"unknown-check":     func(c *SwarmContract) { c.Criteria[0].CheckKind = "model_score" },
		"missing-eval":      func(c *SwarmContract) { c.Criteria[0].CheckKind = "deterministic" },
		"too-many-repairs":  func(c *SwarmContract) { c.MaxRepairs = 3 },
		"unsafe-write-path": func(c *SwarmContract) {
			c.Tasks = []SwarmTaskBinding{{TaskID: "impl", AllowedWritePaths: []string{"../outside"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := swarmTestContract(run.Goal)
			mutate(&c)
			if err := validateSwarmContract(run, &c); !errors.Is(err, ErrSwarmInvalid) {
				t.Fatalf("不合法契约应拒绝：%v", err)
			}
		})
	}
}

func TestSwarmContractFreezeIdempotencyAndReplay(t *testing.T) {
	h := newDispatchHarness(t)
	run := swarmTestRun(t, h, nil, nil)
	// 该测试只核验命令账本，不触发后台派发。
	run.Status = RunCompleted
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	request := SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1, Contract: swarmTestContract(run.Goal)}
	proposed, err := h.service.ProposeSwarmContract(run.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := h.service.ProposeSwarmContract(run.ID, request)
	if err != nil || duplicate.Version != proposed.Version {
		t.Fatalf("重复提出不能追加事件或要求新版本：%v", err)
	}
	conflict := request
	conflict.Contract.MaxRepairs = 1
	if _, err := h.service.ProposeSwarmContract(run.ID, conflict); !errors.Is(err, ErrRunContentMismatch) {
		t.Fatalf("同一操作不能改内容：%v", err)
	}
	stale := SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1}
	if _, err := h.service.AcceptSwarmContract(run.ID, proposed.Swarm.Proposal.ID, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期接受必须冲突：%v", err)
	}
	accept := SwarmRequest{ID: uuid.NewString(), Version: proposed.Version, SchemaVersion: 1}
	frozen, err := h.service.AcceptSwarmContract(run.ID, proposed.Swarm.Proposal.ID, accept)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err = h.service.AcceptSwarmContract(run.ID, proposed.Swarm.Proposal.ID, accept)
	if err != nil || duplicate.Version != frozen.Version {
		t.Fatalf("重复接受必须幂等：%v", err)
	}
	if frozen.Swarm.ContractHash != goalHash(frozen.Swarm.Contract) || frozen.Swarm.Contract.OriginalGoal != run.Goal {
		t.Fatal("冻结身份或原始要求错误")
	}
	if _, err := h.service.ProposeSwarmContract(run.ID, SwarmRequest{ID: uuid.NewString(), Version: frozen.Version, SchemaVersion: 1, Contract: request.Contract}); !errors.Is(err, ErrSwarmInvalid) {
		t.Fatalf("冻结后不能替换契约：%v", err)
	}
	raw, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip Run
	if err := json.Unmarshal(raw, &roundtrip); err != nil || roundtrip.Swarm == nil || roundtrip.Swarm.ContractHash != frozen.Swarm.ContractHash {
		t.Fatalf("快照不能丢失冻结事实：%v", err)
	}
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := &Run{ID: run.ID}
	for _, event := range events {
		if event.RunID == run.ID {
			applyJournalEventToRun(rebuilt, event)
		}
	}
	if rebuilt.Swarm == nil || rebuilt.Swarm.ContractHash != frozen.Swarm.ContractHash || rebuilt.Swarm.Operations[accept.ID] == "" {
		t.Fatal("journal 重建丢失契约或接受幂等记录")
	}
}

func TestSwarmReadyNeedsAcceptedProducerEvidence(t *testing.T) {
	team := testTeam()
	producer := Task{ID: "impl", MemberID: "dev", WriteLock: TaskWriteLockExclusive, Status: TaskCompleted}
	consumer := Task{ID: "consumer", MemberID: "dev", WriteLock: TaskWriteLockExclusive, DependsOn: []string{"impl"}}
	run := &Run{Goal: "goal", Tasks: []Task{producer, consumer}, Swarm: newSwarm("chain")}
	if SwarmReady(run, team, &run.Tasks[0]) {
		t.Fatal("未接受契约不得派发写任务")
	}
	c := swarmTestContract(run.Goal)
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}, EvalSpecIDs: []string{"behavior"}}}
	c.EvalSpecs = []SwarmEvalSpec{{ID: "behavior", CriterionIDs: []string{"original"}, Command: []string{"go", "test"}}}
	run.Swarm.Contract = &c
	run.Swarm.ContractHash = goalHash(c)
	if SwarmReady(run, team, &run.Tasks[1]) {
		t.Fatal("Task completed 不能代替产物证据")
	}
	run.Swarm.Artifacts = []SwarmArtifact{{ID: "candidate", TaskID: "impl", Validity: "fresh"}}
	run.Swarm.Evals = []SwarmEval{{ID: "eval", ArtifactID: "candidate", EvalSpecID: "behavior", Status: "failed", Validity: "fresh"}}
	if SwarmReady(run, team, &run.Tasks[1]) {
		t.Fatal("失败检查不能释放依赖")
	}
	run.Swarm.Evals[0].Status = "passed"
	run.Swarm.Evals[0].Validity = "stale"
	if SwarmReady(run, team, &run.Tasks[1]) {
		t.Fatal("过期通过不能释放依赖")
	}
	run.Swarm.Evals[0].Validity = "fresh"
	if !SwarmReady(run, team, &run.Tasks[1]) {
		t.Fatal("有效产物证据应释放依赖")
	}
}

func TestSwarmUncapturedWriteCannotPassAsConsultation(t *testing.T) {
	c := swarmTestContract("goal")
	run := &Run{Goal: "goal", Tasks: []Task{{ID: "impl", MemberID: "dev", WriteLock: TaskWriteLockExclusive, Status: TaskCompleted}}, Swarm: newSwarm("chain")}
	run.Swarm.Contract = &c
	run.Swarm.ContractHash = goalHash(c)
	if swarmRunPassed(run) {
		t.Fatal("写任务尚未采集产物，不能因缺少检查而当作纯咨询放行")
	}
}

func TestSwarmReviewFailureReachesRepairAndKeepsFrozenContract(t *testing.T) {
	h := newDispatchHarness(t)
	c := swarmTestContract("修复队列恢复")
	c.RequireIndependentReview = true
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := swarmTestRun(t, h, &c, []Task{
		{ID: "impl", MemberID: "dev", Prompt: "fix", WriteLock: TaskWriteLockExclusive, Status: TaskCompleted},
		{ID: "review", MemberID: "rev", Review: reviewFlag(true), WriteLock: TaskWriteLockNone, Status: TaskCompleted, ResultDetail: &TaskResult{ReviewVerdict: "failed"}, Result: "持久化存在缺陷"},
	})
	run.Swarm.Artifacts = []SwarmArtifact{{ID: "candidate", TaskID: "impl", Validity: "fresh", ReviewTaskID: "review"}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	h.service.SwarmRunner = &swarmeval.Runner{Root: t.TempDir()}
	state := h.service.stateFor(run.ID)
	h.service.passSwarm(state)
	h.service.passSwarm(state)
	fresh := h.store.Run(run.ID)
	if len(fresh.Swarm.Findings) != 1 || fresh.Swarm.Findings[0].RepairTaskID == "" {
		t.Fatal("重复审查失败应进入一次修复，不能在 finding 去重处无限循环")
	}
	if fresh.Swarm.ContractHash != goalHash(fresh.Swarm.Contract) || len(fresh.Swarm.Contract.Tasks) != len(c.Tasks) {
		t.Fatal("修复任务不能修改已冻结的验收契约")
	}
}

func TestSwarmArtifactRejectsAuthorSelfReview(t *testing.T) {
	c := swarmTestContract("goal")
	c.RequireIndependentReview = true
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := &Run{Goal: "goal", Tasks: []Task{
		{ID: "impl", MemberID: "dev", DispatchSessionID: "same-session", Status: TaskCompleted},
		{ID: "review", MemberID: "other-member", DispatchSessionID: "same-session", Status: TaskCompleted, Review: reviewFlag(true), ResultDetail: &TaskResult{ReviewVerdict: "passed"}},
	}, Swarm: newSwarm("chain")}
	run.Swarm.Contract = &c
	run.Swarm.ContractHash = goalHash(c)
	a := &SwarmArtifact{ID: "candidate", TaskID: "impl", Validity: "fresh", ReviewTaskID: "review"}
	if swarmArtifactPassed(run, a) {
		t.Fatal("同一实际会话换成员名称不能成为独立审查")
	}
}

func TestSwarmRepairBudgetSurvivesAmendChain(t *testing.T) {
	h := newDispatchHarness(t)
	c := swarmTestContract("修复队列恢复")
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	ancestor := swarmTestRun(t, h, &c, nil)
	ancestor.Swarm.Findings = []SwarmFinding{{CriterionID: "original", RepairTaskID: "repair1"}, {CriterionID: "original", RepairTaskID: "repair2"}}
	if err := h.store.SaveRun(ancestor); err != nil {
		t.Fatal(err)
	}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", Prompt: "fix", Status: TaskCompleted, WriteLock: TaskWriteLockExclusive}})
	run.Swarm.Findings = []SwarmFinding{{ID: "finding", TaskID: "impl", CriterionID: "original", Status: "open", Signature: "new-error"}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if h.service.planSwarmRepair(h.service.stateFor(run.ID), run) {
		t.Fatal("新 Run 不能清空执行链已用完的两次修复预算")
	}
	if fresh := h.store.Run(run.ID); len(fresh.Tasks) != 1 || !strings.Contains(fresh.Swarm.Reason, "上限") {
		t.Fatalf("修复预算耗尽应保留任务并解释停靠：%+v", fresh.Swarm)
	}
}

func TestSwarmRequiredReviewCannotBeDisabled(t *testing.T) {
	run := &Run{Goal: "goal"}
	c := swarmTestContract(run.Goal)
	c.Criteria = append(c.Criteria, SwarmCriterion{ID: "review", Text: "独立审查通过", CheckKind: "review", Required: true})
	if err := validateSwarmContract(run, &c); !errors.Is(err, ErrSwarmInvalid) {
		t.Fatalf("必要 review 条件不能与关闭独立审查矛盾：%v", err)
	}
}

func TestSwarmIntegrationReviewExcludesEveryInputAuthor(t *testing.T) {
	c := swarmTestContract("goal")
	c.RequireIndependentReview = true
	run := &Run{Swarm: newSwarm("chain"), Tasks: []Task{
		{ID: "first", MemberID: "first-author", DispatchSessionID: "first-session"},
		{ID: "second", MemberID: "second-author", DispatchSessionID: "second-session"},
		{ID: "review", MemberID: "second-author", DispatchSessionID: "review-session", Status: TaskCompleted, ResultDetail: &TaskResult{ReviewVerdict: "passed"}},
	}}
	run.Swarm.Contract = &c
	run.Swarm.ContractHash = goalHash(c)
	run.Swarm.Artifacts = []SwarmArtifact{
		{ID: "a", TaskID: "first", Kind: "task", Validity: "fresh"},
		{ID: "b", TaskID: "second", Kind: "task", Validity: "fresh"},
		{ID: "i", TaskID: "integration", Kind: "integration", Inputs: []string{"a", "b"}, ReviewTaskID: "review", Validity: "fresh"},
	}
	if swarmArtifactPassed(run, &run.Swarm.Artifacts[2]) {
		t.Fatal("任一输入作者都不能审查集成候选")
	}
	run.Tasks[2].MemberID = "independent"
	run.Tasks[2].DispatchSessionID = "first-session"
	if swarmArtifactPassed(run, &run.Swarm.Artifacts[2]) {
		t.Fatal("集成审查不能复用任一输入作者的实际会话")
	}
	run.Tasks[2].DispatchSessionID = "independent-session"
	if !swarmArtifactPassed(run, &run.Swarm.Artifacts[2]) {
		t.Fatal("全部输入作者之外的独立审查应可通过")
	}
}

func TestSwarmRecheckRejectsUnknownTarget(t *testing.T) {
	h := newDispatchHarness(t)
	c := swarmTestContract("修复队列恢复")
	c.EvalSpecs = []SwarmEvalSpec{{ID: "check", CriterionIDs: []string{"original"}, Command: []string{"go", "test"}}}
	run := swarmTestRun(t, h, &c, nil)
	run.Status = RunCompleted
	run.Swarm.Artifacts = []SwarmArtifact{{ID: "candidate", TaskID: "integration", Kind: "integration", Validity: "fresh"}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	_, err := h.service.RequestSwarmEval(run.ID, "nonexistent-eval", SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1})
	if !errors.Is(err, ErrSwarmInvalid) {
		t.Fatalf("非法复验目标不能回退到默认候选发起新评测：%v", err)
	}
	if fresh := h.store.Run(run.ID); len(fresh.Swarm.Evals) != 0 || fresh.Version != run.Version {
		t.Fatal("拒绝复验不得产生预算或状态副作用")
	}
}

func TestSwarmGateRejectsChangedFrozenContract(t *testing.T) {
	team := testTeam()
	c := swarmTestContract("goal")
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := &Run{Goal: "goal", Tasks: []Task{{ID: "impl", MemberID: "dev", WriteLock: TaskWriteLockExclusive}}, Swarm: newSwarm("chain")}
	run.Swarm.Contract = &c
	run.Swarm.ContractHash = goalHash(c)
	run.Swarm.Contract.MaxEvals++
	if SwarmReady(run, team, &run.Tasks[0]) {
		t.Fatal("冻结契约身份不一致时不得派发")
	}
	if swarmRunPassed(run) {
		t.Fatal("冻结契约身份不一致时不得签发执行通过")
	}
}

func swarmTestGitArtifact(t *testing.T, runner *swarmeval.Runner, id string) *swarmeval.Artifact {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("config", "user.name", "Swarm test")
	runGit("config", "user.email", "swarm@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "source.txt")
	runGit("commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := runner.Capture(context.Background(), id, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSwarmRepairDoesNotRecaptureOriginalTask(t *testing.T) {
	h := newDispatchHarness(t)
	runner := &swarmeval.Runner{Root: t.TempDir()}
	c := swarmTestContract("修复队列恢复")
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := swarmTestRun(t, h, &c, []Task{
		{ID: "impl", MemberID: "dev", Prompt: "fix", WriteLock: TaskWriteLockExclusive, Status: TaskCompleted, Attempt: 1},
		{ID: "repair", MemberID: "dev", Prompt: "repair", WriteLock: TaskWriteLockExclusive, Status: TaskPlanned, Attempt: 1},
	})
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("swarm-artifact:"+run.ID+":impl"+fmt.Sprint(1))).String()
	a := swarmTestGitArtifact(t, runner, id+"_source")
	frozen, err := runner.Detach(context.Background(), id, a)
	if err != nil {
		t.Fatal(err)
	}
	h.resolver.setCwd(memberOf(h.team, "dev").SessionID, a.SourceDir)
	run.Swarm.Artifacts = []SwarmArtifact{swarmArtifactRef(&run.Tasks[0], frozen)}
	run.Swarm.Findings = []SwarmFinding{{ID: "finding", TaskID: "impl", CriterionID: "original", RepairTaskID: "repair", Status: "repairing"}}
	run.Swarm.Operations[id] = "captured"
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	h.service.SwarmRunner = runner
	h.service.passSwarm(h.service.stateFor(run.ID))
	fresh := h.store.Run(run.ID)
	if fresh.Swarm.Operations[id] != "captured" || len(fresh.Swarm.Artifacts) != 1 || fresh.Swarm.Status == "blocked" {
		t.Fatalf("在途修复不能触发原任务重新捕获：%+v", fresh.Swarm)
	}
}

func TestSwarmSingleCandidateIntegrationConsumesRequestedOperation(t *testing.T) {
	h := newDispatchHarness(t)
	runner := &swarmeval.Runner{Root: t.TempDir()}
	a := swarmTestGitArtifact(t, runner, "single-candidate")
	c := swarmTestContract("修复队列恢复")
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", Status: TaskCompleted, WriteLock: TaskWriteLockExclusive}})
	run.Swarm.Artifacts = []SwarmArtifact{swarmArtifactRef(&run.Tasks[0], a)}
	h.service.SwarmRunner = runner
	requestID := uuid.NewString()
	// 直接登记已验证的意图，单独测试消费，避免与命令自动启动的 loop 并行推进。
	run.Swarm.Operations["integrate_"+requestID] = a.ID
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	h.service.passSwarm(h.service.stateFor(run.ID))
	fresh := h.store.Run(run.ID)
	if fresh.Swarm.Operations["integrate_"+requestID] != "" || fresh.Swarm.Operations[requestID] != "integrated" || swarmArtifact(fresh, requestID) == nil {
		t.Fatalf("单候选也必须消费用户指定的集成意图：%+v", fresh.Swarm)
	}
}

func TestSwarmServiceCandidateEvalRepairReviewIntegration(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Fatal("此集成测试需要 make")
	}
	h := newDispatchHarness(t)
	runner := &swarmeval.Runner{Root: t.TempDir()}
	seed := swarmTestGitArtifact(t, runner, "seed-for-service")
	// 独立执行器运行真实断言；仅成员回复由 fixture 提供，不调用模型。
	if err := os.WriteFile(filepath.Join(seed.SourceDir, "Makefile"), []byte("check:\n\t@test \"$$(cat source.txt)\" = fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.resolver.setCwd("s-dev", seed.SourceDir)
	h.service.SwarmRunner = runner
	h.service.ExecutionSessions = &fakeExecutionSessionProvisioner{resolver: h.resolver, sessions: map[string]string{}}
	c := swarmTestContract("修复队列恢复")
	c.RequireIndependentReview = true
	c.Criteria = append(c.Criteria, SwarmCriterion{ID: "behavior", Text: "真实命令检查通过", CheckKind: "deterministic", Required: true})
	c.EvalSpecs = []SwarmEvalSpec{{ID: "behavior", CriterionIDs: []string{"behavior"}, Command: []string{"make", "check"}, TimeoutSeconds: 10}}
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", CriterionIDs: []string{"behavior"}, AllowedWritePaths: []string{"."}, EvalSpecIDs: []string{"behavior"}}}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", DispatchSessionID: "s-dev", Prompt: "fix behavior", Status: TaskCompleted, WriteLock: TaskWriteLockExclusive, Attempt: 1, BaseRevision: seed.BaseRevision}})
	state := h.service.stateFor(run.ID)
	observedFailure, repaired, reviews, integrated := false, false, 0, false
	for step := 0; step < 30; step++ {
		h.service.passSwarm(state)
		fresh := h.store.Run(run.ID)
		for _, evaluation := range fresh.Swarm.Evals {
			if evaluation.Status == "failed" {
				observedFailure = true
			}
		}
		changed := false
		for i := range fresh.Tasks {
			task := &fresh.Tasks[i]
			if task.Status != TaskPlanned {
				continue
			}
			if strings.HasPrefix(task.ID, "swfix-") {
				if !observedFailure {
					t.Fatal("没有真实失败证据就创建了修复")
				}
				if err := os.WriteFile(filepath.Join(seed.SourceDir, "source.txt"), []byte("fixed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				task.Status, task.DispatchSessionID = TaskCompleted, "s-dev"
				task.Result = "已修复"
				repaired, changed = true, true
			} else if strings.HasPrefix(task.ID, "swrev-") {
				task.Status, task.DispatchSessionID = TaskCompleted, "independent-"+task.ID
				task.ResultDetail = &TaskResult{ReviewVerdict: ReviewVerdictPassed}
				reviews++
				changed = true
			}
		}
		if changed {
			if err := h.store.SaveRun(fresh); err != nil {
				t.Fatal(err)
			}
			fresh = h.store.Run(run.ID)
		}
		for _, artifact := range fresh.Swarm.Artifacts {
			if artifact.Kind == "integration" {
				integrated = true
			}
		}
		if swarmRunPassed(fresh) {
			if !observedFailure || !repaired || reviews < 2 || !integrated {
				t.Fatalf("闭环缺阶段：failure=%v repair=%v reviews=%d integration=%v", observedFailure, repaired, reviews, integrated)
			}
			if fresh.Swarm.ContractHash != goalHash(fresh.Swarm.Contract) {
				t.Fatal("自动修复改变了冻结契约")
			}
			events, err := h.journal.Replay()
			if err != nil {
				t.Fatal(err)
			}
			rebuilt := &Run{ID: run.ID, TeamID: run.TeamID, Goal: run.Goal}
			for _, event := range events {
				if event.RunID == run.ID {
					applyJournalEventToRun(rebuilt, event)
				}
			}
			if rebuilt.Swarm == nil || rebuilt.Swarm.ContractHash != fresh.Swarm.ContractHash || len(rebuilt.Swarm.RepairBindings) != 1 {
				t.Fatal("重放未恢复冻结契约或修复绑定")
			}
			for _, task := range fresh.Tasks {
				if !strings.HasPrefix(task.ID, "swfix-") && !strings.HasPrefix(task.ID, "swrev-") {
					continue
				}
				restored := findTaskIn(rebuilt, task.ID)
				if restored == nil || restored.Prompt != task.Prompt || restored.MemberID != task.MemberID || restored.ExecutionThreadID != task.ExecutionThreadID || restored.WriteLock != task.WriteLock {
					t.Fatalf("审查/修复完整任务重放丢失：%+v", restored)
				}
			}
			h.service.passFold(state)
			if got := h.store.Run(run.ID); got.Status != RunCompleted {
				t.Fatalf("全部有效证据后执行仍未结束：%s %s", got.Status, got.StopReason)
			}
			return
		}
		if fresh.Status == RunCompleted {
			t.Fatal("全部证据未通过时提前执行结束")
		}
	}
	fresh := h.store.Run(run.ID)
	t.Fatalf("30 次确定性推进未收敛：status=%s reason=%s tasks=%+v evals=%+v", fresh.Swarm.Status, fresh.Swarm.Reason, fresh.Tasks, fresh.Swarm.Evals)
}

func TestSwarmReviewDispatchUsesDedicatedCandidateSession(t *testing.T) {
	testSwarmReviewDispatch(t, false)
}

func TestSwarmIntegrationReviewCannotMutateAcceptedCandidate(t *testing.T) {
	testSwarmReviewDispatch(t, true)
}

func testSwarmReviewDispatch(t *testing.T, tamperIntegration bool) {
	t.Helper()
	h := newDispatchHarness(t)
	runner := &swarmeval.Runner{Root: t.TempDir()}
	source := swarmTestGitArtifact(t, runner, "review-source")
	a, err := runner.Detach(context.Background(), "review-candidate", source)
	if err != nil {
		t.Fatal(err)
	}
	c := swarmTestContract("修复队列恢复")
	c.RequireIndependentReview = true
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", DispatchSessionID: "s-dev", WriteLock: TaskWriteLockExclusive, Status: TaskCompleted}})
	run.Swarm.Artifacts = []SwarmArtifact{swarmArtifactRef(&run.Tasks[0], a)}
	run.Swarm.Operations[a.ID] = "captured"
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	h.service.SwarmRunner = runner
	h.service.ExecutionSessions = &fakeExecutionSessionProvisioner{resolver: h.resolver, sessions: map[string]string{}}
	h.service.startLoop(h.service.stateFor(run.ID))
	completed := map[string]bool{}
	for reviewNumber := 0; reviewNumber < 2; reviewNumber++ {
		var review Task
		var artifact SwarmArtifact
		waitFor(t, "独立候选审查派发", func() bool {
			fresh := h.store.Run(run.ID)
			for _, task := range fresh.Tasks {
				if !strings.HasPrefix(task.ID, "swrev-") || completed[task.ID] || task.DispatchSessionID == "" {
					continue
				}
				if entry, ok := h.queue.firstEntry(task.DispatchSessionID); !ok || entry.submissionID != task.SubmissionID {
					continue
				}
				if candidate := swarmReviewArtifact(fresh, task.ID); candidate != nil {
					review, artifact = task, *candidate
					return true
				}
			}
			return false
		})
		frozen, err := runner.LoadArtifact(context.Background(), artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		if review.DispatchSessionID == "s-rev" || review.DispatchSessionID == "s-dev" {
			t.Fatalf("审查错误复用了成员原会话：%s", review.DispatchSessionID)
		}
		if actual := h.resolver.Cwd(review.DispatchSessionID); actual != frozen.Directory {
			t.Fatalf("审查必须在对应冻结候选目录：%q vs %q", actual, frozen.Directory)
		}
		if actual := h.resolver.Cwd("s-rev"); actual != "/tmp/rev" {
			t.Fatalf("创建独立审查不得换绑成员原目录：%q", actual)
		}
		if !strings.Contains(review.SentText, artifact.ContentHash) || !strings.Contains(review.SentText, frozen.Directory) {
			t.Fatal("审查实际派发未绑定候选目录和内容身份")
		}
		completed[review.ID] = true
		if tamperIntegration && artifact.Kind == "integration" {
			if err := os.WriteFile(filepath.Join(frozen.Directory, "source.txt"), []byte("changed-during-review\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		h.queue.setStatus(review.DispatchSessionID, review.SubmissionID, SubStatusAccepted, "")
		h.resolver.setSession(review.DispatchSessionID, newFakeSession(review.DispatchSessionID, frozen.Directory, turnCompleted(review.SentText, "逐项检查通过\n<team-review>passed</team-review>")))
	}
	if tamperIntegration {
		waitFor(t, "被修改的集成候选阻塞", func() bool {
			fresh := h.store.Run(run.ID)
			return fresh.Swarm.Status == "blocked" || fresh.Status == RunCompleted
		})
		fresh := h.store.Run(run.ID)
		if fresh.Status == RunCompleted || swarmRunPassed(fresh) {
			t.Fatal("Reviewer 修改了最终候选，旧通过证据不能使执行结束")
		}
		return
	}
	waitFor(t, "审查与集成收敛", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
	fresh := h.store.Run(run.ID)
	if !swarmRunPassed(fresh) || len(completed) != 2 {
		t.Fatalf("工作候选与集成候选应分别独立审查：%+v", fresh.Swarm)
	}
}

type swarmProvisionerFunc func(string, string, *TeamMember, string) (string, error)

func (f swarmProvisionerFunc) EnsureExecutionSession(teamID, runID string, member *TeamMember, cwd string) (string, error) {
	return f(teamID, runID, member, cwd)
}

func TestSwarmUnavailableReviewSessionDoesNotFallbackToMemberChat(t *testing.T) {
	h := newDispatchHarness(t)
	runner := &swarmeval.Runner{Root: t.TempDir()}
	source := swarmTestGitArtifact(t, runner, "unavailable-review-source")
	a, err := runner.Detach(context.Background(), "unavailable-review-candidate", source)
	if err != nil {
		t.Fatal(err)
	}
	c := swarmTestContract("修复队列恢复")
	c.RequireIndependentReview = true
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", DispatchSessionID: "s-dev", WriteLock: TaskWriteLockExclusive, Status: TaskCompleted}})
	run.Swarm.Artifacts = []SwarmArtifact{swarmArtifactRef(&run.Tasks[0], a)}
	run.Swarm.Operations[a.ID] = "captured"
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	h.service.SwarmRunner = runner
	h.service.ExecutionSessions = swarmProvisionerFunc(func(_, _ string, _ *TeamMember, cwd string) (string, error) {
		id := "missing-dedicated-review-session"
		h.resolver.setCwd(id, cwd)
		h.resolver.setUnopenable(id, errors.New("session not found"))
		return id, nil
	})
	h.service.startLoop(h.service.stateFor(run.ID))
	waitFor(t, "专用审查会话不可用的结算", func() bool {
		fresh := h.store.Run(run.ID)
		for _, task := range fresh.Tasks {
			if strings.HasPrefix(task.ID, "swrev-") && task.Status != TaskPlanned && task.Status != TaskReady {
				return true
			}
		}
		return false
	})
	if entry, ok := h.queue.firstEntry("s-rev"); ok {
		t.Fatalf("专用会话不可用不得向原成员聊天发送审查：%+v", entry)
	}
	if fresh := h.store.Team(h.team.ID); memberOf(fresh, "rev").SessionID != "s-rev" {
		t.Fatal("独立审查恢复不得改写成员控制会话")
	}
}

func TestSwarmReviewFormatRepairStaysWithCandidateContext(t *testing.T) {
	h := newDispatchHarness(t)
	runner := &swarmeval.Runner{Root: t.TempDir()}
	source := swarmTestGitArtifact(t, runner, "format-review-source")
	a, err := runner.Detach(context.Background(), "format-review-candidate", source)
	if err != nil {
		t.Fatal(err)
	}
	c := swarmTestContract("修复队列恢复")
	c.RequireIndependentReview = true
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", DispatchSessionID: "s-dev", WriteLock: TaskWriteLockExclusive, Status: TaskCompleted}})
	run.Swarm.Artifacts = []SwarmArtifact{swarmArtifactRef(&run.Tasks[0], a)}
	run.Swarm.Operations[a.ID] = "captured"
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	h.service.SwarmRunner = runner
	h.service.ExecutionSessions = &fakeExecutionSessionProvisioner{resolver: h.resolver, sessions: map[string]string{}}
	h.service.startLoop(h.service.stateFor(run.ID))
	reviewID := "swrev-" + a.ID
	var review Task
	waitFor(t, "首次独立审查派发", func() bool {
		task := findTaskIn(h.store.Run(run.ID), reviewID)
		if task != nil && task.DispatchSessionID != "" {
			if _, ok := h.queue.firstEntry(task.DispatchSessionID); ok {
				review = *task
				return true
			}
		}
		return false
	})
	h.queue.setStatus(review.DispatchSessionID, review.SubmissionID, SubStatusAccepted, "")
	h.resolver.setSession(review.DispatchSessionID, newFakeSession(review.DispatchSessionID, a.Directory, turnCompleted(review.SentText, "逐项检查未发现问题，但忘记格式标记。")))
	var formatRepair Task
	t.Cleanup(func() {
		if t.Failed() {
			fresh := h.store.Run(run.ID)
			t.Logf("格式修复停靠：status=%s swarm=%s reason=%s tasks=%+v", fresh.Status, fresh.Swarm.Status, fresh.Swarm.Reason, fresh.Tasks)
			for _, task := range fresh.Tasks {
				if task.ResultDetail != nil {
					t.Logf("task=%s verdict=%q", task.ID, task.ResultDetail.ReviewVerdict)
				}
			}
			t.Logf("contract=%v artifact-review=%s", fresh.Swarm.Contract != nil, fresh.Swarm.Artifacts[0].ReviewTaskID)
		}
	})
	waitFor(t, "一次审查格式修复派发", func() bool {
		fresh := h.store.Run(run.ID)
		for _, task := range fresh.Tasks {
			if (task.ID == "swfmt-"+a.ID || strings.HasPrefix(task.ID, "revfix-"+reviewID+"-")) && task.DispatchSessionID != "" {
				if _, ok := h.queue.firstEntry(task.DispatchSessionID); ok {
					formatRepair = task
					return true
				}
			}
		}
		return false
	})
	if formatRepair.DispatchSessionID == "s-rev" || h.resolver.Cwd(formatRepair.DispatchSessionID) != a.Directory {
		t.Fatal("审查格式修复必须保留原候选的独立上下文，不能派回成员聊天")
	}
	fresh := h.store.Run(run.ID)
	for _, finding := range fresh.Swarm.Findings {
		if finding.RepairTaskID != "" {
			t.Fatal("裁决格式缺失不得直接驱动功能修改")
		}
	}
	h.queue.setStatus(formatRepair.DispatchSessionID, formatRepair.SubmissionID, SubStatusAccepted, "")
	h.resolver.setSession(formatRepair.DispatchSessionID, newFakeSession(formatRepair.DispatchSessionID, a.Directory, turnCompleted(formatRepair.SentText, "原候选逐项检查通过\n<team-review>passed</team-review>")))
	waitFor(t, "格式修复裁决成为候选证据", func() bool {
		current := h.store.Run(run.ID)
		candidate := swarmArtifact(current, a.ID)
		return candidate != nil && swarmArtifactPassed(current, candidate)
	})
	fresh = h.store.Run(run.ID)
	formats := 0
	for _, task := range fresh.Tasks {
		if strings.HasPrefix(task.ID, "swfmt-") || strings.HasPrefix(task.ID, "revfix-"+reviewID+"-") {
			formats++
		}
	}
	if formats != 1 {
		t.Fatalf("每个候选最多一次格式修复，实际 %d", formats)
	}
}

func TestSwarmGoalEvidenceHashKeepsLegacyIdentity(t *testing.T) {
	run := &Run{ID: "legacy-run", Status: RunCompleted}
	// 旧版六字段对象的固定 SHA256；增加 Swarm 支持不能改变存量确认的证据身份。
	const legacy = "327807bec23d79b88a2d4b730120afbeeef5930cf7a7b1416fce8dfd3d26acb6"
	if got := GoalEvidenceHash(run); got != legacy {
		t.Fatalf("旧目标证据身份发生漂移：%s", got)
	}
	run.Version = 42
	run.Goals = map[string]*GoalRecord{"run": {Reason: "已有用户确认"}}
	if got := GoalEvidenceHash(run); got != legacy {
		t.Fatalf("版本或确认记录不能进入执行证据哈希：%s", got)
	}
	run.Swarm = newSwarm(run.ID)
	if got := GoalEvidenceHash(run); got == legacy {
		t.Fatal("启用 Swarm 后必须纳入新的证据维度")
	}
}

func TestSwarmCriteriaUseExistingGoalAssessment(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	run.Tasks[0].WriteLock = TaskWriteLockNone
	c := swarmTestContract(run.Goal)
	c.Criteria = append(c.Criteria,
		SwarmCriterion{ID: "experience", Text: "手机操作体验符合预期", CheckKind: "human", Required: true},
		SwarmCriterion{ID: "optional", Text: "辅助记录", CheckKind: "human", Required: false},
		SwarmCriterion{ID: "other_issue", Text: "另一事项的条件", CheckKind: "human", Required: true, IssueIDs: []string{"another-issue"}},
	)
	run.Swarm = newSwarm(run.ID)
	run.Swarm.Contract, run.Swarm.ContractHash = &c, goalHash(c)
	run.Swarm.ScopeHash = RunGoalScope(run).Hash
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	scope := RunGoalScope(run)
	out := h.service.GoalOutcome(h.store.Run(run.ID), scope)
	if len(out.Contract.Criteria) != 2 || out.Contract.Criteria[1].ID != "experience" || !out.CanConfirm {
		t.Fatalf("Swarm 条件未加入原目标视图或跨scope泄漏：%+v", out)
	}
	missing := goalRequest(h.store.Run(run.ID), scope, "assess")
	missing.States = map[string]string{"original": "met"}
	if _, err := h.service.ReviewGoal(run.ID, scope, missing); !errors.Is(err, ErrGoalReviewInvalid) {
		t.Fatalf("不能遗漏新增的必要条件：%v", err)
	}
	request := goalRequest(h.store.Run(run.ID), scope, "assess")
	request.States = map[string]string{"original": "met", "experience": "met"}
	confirmed, err := h.service.ReviewGoal(run.ID, scope, request)
	if err != nil {
		t.Fatal(err)
	}
	if actual := EvaluateGoal(confirmed, h.team, scope); actual.Status != "satisfied" || len(actual.Contract.Criteria) != 2 || actual.Contract.Criteria[1].State != "met" {
		t.Fatalf("应通过既有接口保留全部条件的用户判断：%+v", actual)
	}
	if len(h.queue.calls) != 0 {
		t.Fatal("确认结果不应派发成员或执行文件操作")
	}
}

func TestSwarmMissingMachineEvidenceBlocksGoalMet(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	run.Tasks[0].WriteLock = TaskWriteLockNone
	c := swarmTestContract(run.Goal)
	c.Criteria = append(c.Criteria, SwarmCriterion{ID: "behavior", Text: "必要行为检查通过", CheckKind: "deterministic", Required: true})
	c.EvalSpecs = []SwarmEvalSpec{{ID: "behavior", CriterionIDs: []string{"behavior"}, Command: []string{"go", "test"}}}
	run.Swarm = newSwarm(run.ID)
	run.Swarm.Contract, run.Swarm.ContractHash = &c, goalHash(c)
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	scope := RunGoalScope(run)
	if out := h.service.GoalOutcome(run, scope); out.CanConfirm || out.Status != "pending_verification" {
		t.Fatalf("机器证据缺失不能供用户盖章：%+v", out)
	}
	request := goalRequest(run, scope, "assess")
	request.States = map[string]string{"original": "met", "behavior": "met"}
	if _, err := h.service.ReviewGoal(run.ID, scope, request); !errors.Is(err, ErrGoalReviewInvalid) {
		t.Fatalf("缺少必要机器证据必须拒绝全部met：%v", err)
	}
	if fresh := h.store.Run(run.ID); fresh.Goals[scope.ID] != nil {
		t.Fatal("拒绝的确认不能留下达成记录")
	}
}

func TestSwarmAcceptedJournalRecoversBeforeDuplicateCommand(t *testing.T) {
	h := newDispatchHarness(t)
	run := swarmTestRun(t, h, nil, nil)
	run.Status = RunCompleted
	proposalID := uuid.NewString()
	c := swarmTestContract(run.Goal)
	if err := validateSwarmContract(run, &c); err != nil {
		t.Fatal(err)
	}
	c.Revision = 1
	run.Swarm.Proposal = &SwarmProposal{ID: proposalID, Contract: c, Source: "user"}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	request := SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1}
	accepted := run.Clone()
	accepted.Swarm.Contract, accepted.Swarm.ContractHash = &c, goalHash(c)
	accepted.Swarm.ScopeHash = RunGoalScope(run).Hash
	accepted.Swarm.Status = "running"
	content := request
	content.Version = 0
	accepted.Swarm.Operations = map[string]string{request.ID: goalHash([]any{"accept", proposalID, content})}
	// 模拟 journal 已持久化，但快照写入之前进程终止。
	if err := h.journal.Append(swarmEvent(accepted, "user")); err != nil {
		t.Fatal(err)
	}
	before, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := h.service.AcceptSwarmContract(run.ID, proposalID, request)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Swarm.ContractHash != accepted.Swarm.ContractHash || recovered.Swarm.Operations[request.ID] == "" || recovered.Swarm.Sequence == 0 {
		t.Fatal("重试前未先恢复已经接受的冻结事实")
	}
	version := recovered.Version
	duplicate, err := h.service.AcceptSwarmContract(run.ID, proposalID, request)
	if err != nil || duplicate.Version != version {
		t.Fatalf("恢复后重复接受不得再写快照：%v", err)
	}
	after, err := h.journal.Replay()
	if err != nil || len(after) != len(before) {
		t.Fatalf("已接受操作不能重复追加事件：%d -> %d %v", len(before), len(after), err)
	}
}

func TestSwarmScopeChangeBlocksOldContract(t *testing.T) {
	team := testTeam()
	run := &Run{ID: "scope-run", Goal: "原始范围", Tasks: []Task{{ID: "answer", MemberID: "dev", WriteLock: TaskWriteLockNone, Status: TaskCompleted}}, Swarm: newSwarm("chain")}
	c := swarmTestContract(run.Goal)
	run.Swarm.Contract, run.Swarm.ContractHash = &c, goalHash(c)
	run.Swarm.ScopeHash = RunGoalScope(run).Hash
	if !SwarmReady(run, team, &run.Tasks[0]) || !swarmCandidatePassed(run) {
		t.Fatal("初始范围有效的咨询契约应可推进")
	}
	run.Tasks = append(run.Tasks, Task{ID: "input-new-feedback", MemberID: "lead", Prompt: "追加一个必须实现的行为", ExecutionContext: ExecutionContextLeadControl, WriteLock: TaskWriteLockNone})
	if SwarmReady(run, team, &run.Tasks[0]) || swarmCandidatePassed(run) || swarmRunPassed(run) {
		t.Fatal("新增原话改变范围后不得使用旧契约继续或通过")
	}
	if !SwarmReady(run, team, &run.Tasks[1]) {
		t.Fatal("范围变化不能阻塞Lead接收新的用户输入")
	}
}

func TestSwarmPreflightPreservesExistingChangesAndFreezesCheckEntrypoint(t *testing.T) {
	h := newDispatchHarness(t)
	runner := &swarmeval.Runner{Root: t.TempDir()}
	seed := swarmTestGitArtifact(t, runner, "preflight-seed")
	notes := filepath.Join(seed.SourceDir, "user-notes.txt")
	if err := os.WriteFile(notes, []byte("用户已有的未提交内容\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	makefile := filepath.Join(seed.SourceDir, "Makefile")
	if err := os.WriteFile(makefile, []byte("check:\n\t@echo check\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.team.ProjectCwd = seed.SourceDir
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	h.service.SwarmRunner = runner
	h.service.Fingerprints = &freshFingerprintStub{head: seed.HeadRevision, content: seed.ContentHash}
	c := swarmTestContract("修复队列恢复")
	c.EvalSpecs = []SwarmEvalSpec{{ID: "check", CriterionIDs: []string{"original"}, Command: []string{"make", "check"}}}
	c.Tasks = []SwarmTaskBinding{{TaskID: "impl", CriterionIDs: []string{"original"}, AllowedWritePaths: []string{"."}, EvalSpecIDs: []string{"check"}}}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", WriteLock: TaskWriteLockExclusive, Status: TaskPlanned, Attempt: 1, BaseRevision: seed.BaseRevision}})
	state := h.service.stateFor(run.ID)
	if err := h.service.preflightSwarmInput(state, run, &run.Tasks[0], seed.SourceDir); err != nil {
		t.Fatal(err)
	}
	fresh := h.store.Run(run.ID)
	if fresh.Swarm.TaskInputs["impl"] == "" {
		t.Fatal("派发前必须保存输入快照")
	}
	if err := os.WriteFile(filepath.Join(seed.SourceDir, "source.txt"), []byte("worker change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := runner.Capture(context.Background(), "preflight-after-worker", seed.SourceDir, seed.BaseRevision)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := h.service.swarmActualChanges(context.Background(), fresh, &fresh.Tasks[0], after)
	if err != nil || len(changes) != 1 || changes[0] != "source.txt" {
		t.Fatalf("实际任务改动不能混入用户已有修改：%v %v", changes, err)
	}
	if err := h.service.swarmProtectedChecks(context.Background(), fresh, &fresh.Tasks[0], after); err != nil {
		t.Fatalf("正常实现改动不应触碰检查入口：%v", err)
	}
	if err := os.WriteFile(makefile, []byte("check:\n\t@true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tampered, err := runner.Capture(context.Background(), "preflight-tampered-check", seed.SourceDir, seed.BaseRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.swarmProtectedChecks(context.Background(), fresh, &fresh.Tasks[0], tampered); err == nil || !strings.Contains(err.Error(), "Makefile") {
		t.Fatalf("任务不能降低冻结的检查入口：%v", err)
	}
	if contents, err := os.ReadFile(notes); err != nil || string(contents) != "用户已有的未提交内容\n" {
		t.Fatalf("拒绝产物不得丢弃用户原改动：%q %v", contents, err)
	}
	if contents, err := os.ReadFile(makefile); err != nil || string(contents) != "check:\n\t@true\n" {
		t.Fatalf("检查失败不得擅自回滚源文件：%q %v", contents, err)
	}
}

func TestSwarmCompletedRechecksDoNotLeaveOrphanedPending(t *testing.T) {
	h := newDispatchHarness(t)
	c := swarmTestContract("修复队列恢复")
	c.EvalSpecs = []SwarmEvalSpec{{ID: "a", CriterionIDs: []string{"original"}, Command: []string{"make", "check"}}, {ID: "b", CriterionIDs: []string{"original"}, Command: []string{"make", "check"}}}
	run := swarmTestRun(t, h, &c, nil)
	run.Status = RunCompleted
	run.Swarm.Artifacts = []SwarmArtifact{{ID: "candidate", TaskID: "integration", Kind: "integration", Validity: "fresh"}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	// 执行器未装配，保持首条pending以确定性模拟进行中的终态复验。
	first, err := h.service.RequestSwarmEval(run.ID, "", SwarmRequest{ID: uuid.NewString(), Version: run.Version, SchemaVersion: 1, ArtifactID: "candidate", EvalSpecID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.service.RequestSwarmEval(run.ID, "", SwarmRequest{ID: uuid.NewString(), Version: first.Version, SchemaVersion: 1, ArtifactID: "candidate", EvalSpecID: "b"})
	if !errors.Is(err, ErrSwarmInvalid) {
		t.Fatalf("终态并发复验应明确拒绝而非遗留无消费者pending: %v", err)
	}
	if len(h.store.Run(run.ID).Swarm.Evals) != 1 {
		t.Fatal("被拒绝的复验不得占用预算或写入pending")
	}
}

func TestSwarmRepairRechecksStaleFindingBeforeAppend(t *testing.T) {
	h := newDispatchHarness(t)
	c := swarmTestContract("修复队列恢复")
	c.Tasks = []SwarmTaskBinding{{TaskID: "*", AllowedWritePaths: []string{"."}}}
	run := swarmTestRun(t, h, &c, []Task{{ID: "impl", MemberID: "dev", Status: TaskCompleted, WriteLock: TaskWriteLockExclusive}})
	run.Swarm.Findings = []SwarmFinding{{ID: uuid.NewString(), TaskID: "impl", CriterionID: "original", Status: "open", Signature: "same", Reason: "assertion"}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	stale := h.store.Run(run.ID)
	if !h.service.planSwarmRepair(h.service.stateFor(run.ID), stale) {
		t.Fatal("首次修复应创建")
	}
	if h.service.planSwarmRepair(h.service.stateFor(run.ID), stale) {
		t.Fatal("旧快照不得重复创建同一修复")
	}
	fresh := h.store.Run(run.ID)
	seen := map[string]bool{}
	for _, task := range fresh.Tasks {
		if seen[task.ID] {
			t.Fatalf("重复任务: %s", task.ID)
		}
		seen[task.ID] = true
	}
	if len(fresh.Swarm.RepairBindings) != 1 {
		t.Fatal("不能重复修复绑定或消耗两次预算")
	}
}
