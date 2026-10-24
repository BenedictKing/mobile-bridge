package team

// context 的单元测试（§13.1 team/context 行）：§7.1 信封版式、来源标记、
// 多依赖拓扑序、MaxContextChars 截断（优先保留最新）、上游信封剥离防指数
// 增长、合成全文 ≤100000 字节（prompt 上限 + 上下文截断联合用例，对齐
// internal/httpd/queue.go validateSubmissionInput 的字节校验）。

import (
	"strings"
	"testing"
)

// completedDep 构造一个已完成、带结果引用的依赖任务。
func completedDep(id, memberID string, ordinal int, result string) Task {
	return Task{
		ID: id, MemberID: memberID, Ordinal: ordinal, Status: TaskCompleted,
		Result: result,
		ResultDetail: &TaskResult{
			SessionID: "s-" + id, TurnID: "t-" + id, Result: result,
			ResultChars: len([]rune(result)),
		},
	}
}

// contextRun 构造带依赖的上下文构建对象。
func contextRun(tasks ...Task) *Run {
	return &Run{ID: "run-1", TeamID: "team-1", Status: RunDispatching, Tasks: tasks}
}

func TestBuildTaskContextNoDepsSendsPromptOnly(t *testing.T) {
	task := Task{ID: "solo", MemberID: "lead", Prompt: "干活", Status: TaskPlanned}
	result := BuildTaskContext(contextRun(task), &task, DefaultPolicy())
	if result.Text != "" || result.InjectedChars != 0 || len(result.Sources) != 0 {
		t.Fatalf("无依赖不应注入上下文: %+v", result)
	}
	if text := ComposeTaskMessage(result.Text, task.Prompt); text != "干活" {
		t.Fatalf("无上下文时最终消息应只有任务本身，实际 %q", text)
	}
	// MaxContextChars ≤ 0：即使有依赖也不注入。
	dep := completedDep("design", "lead", 0, "分析结果")
	taskWithDep := Task{ID: "impl", MemberID: "dev", Prompt: "实现", DependsOn: []string{"design"}, Status: TaskPlanned}
	result = BuildTaskContext(contextRun(dep, taskWithDep), &taskWithDep, TeamPolicy{})
	if result.Text != "" {
		t.Fatalf("MaxContextChars=0 不应注入上下文，实际 %q", result.Text)
	}
}

func TestBuildTaskContextEnvelopeMatchesPlanSample(t *testing.T) {
	// §7.1 的示例版式，逐行对照。
	design := completedDep("task-reviewer-1", "reviewer", 0, "发现两个错误处理问题。")
	task := Task{ID: "fix", MemberID: "dev", Prompt: "请修复上述问题，并说明修改了哪些文件。", DependsOn: []string{"task-reviewer-1"}, Status: TaskPlanned}
	result := BuildTaskContext(contextRun(design, task), &task, DefaultPolicy())
	want := `[Team context]
来源成员：reviewer
来源任务：task-reviewer-1
内容可能已截断，完整内容请打开原会话查看。

发现两个错误处理问题。

[Assigned task]
请修复上述问题，并说明修改了哪些文件。`
	if got := ComposeTaskMessage(result.Text, task.Prompt); got != want {
		t.Fatalf("合成文本与 §7.1 版式不符:\n got: %q\nwant: %q", got, want)
	}
	if result.InjectedChars != len([]rune(result.Text)) {
		t.Fatalf("InjectedChars 应记录实际注入长度，实际 %d vs %d", result.InjectedChars, len([]rune(result.Text)))
	}
	if len(result.Sources) != 1 || result.Sources[0].TaskID != "task-reviewer-1" || result.Sources[0].MemberID != "reviewer" {
		t.Fatalf("来源标记错误: %+v", result.Sources)
	}
	if result.Sources[0].Truncated {
		t.Fatal("未触上限不应标记截断")
	}
}

func TestBuildTaskContextOrdersDepsTopologically(t *testing.T) {
	// 故意乱序声明，依赖按 Ordinal 排列：design(0) → implement(1)。
	run := contextRun(
		completedDep("implement", "dev", 1, "实现完成"),
		completedDep("design", "lead", 0, "设计完成"),
		Task{ID: "review", MemberID: "rev", Prompt: "检查", DependsOn: []string{"design", "implement"}, Status: TaskPlanned},
	)
	task := &run.Tasks[2]
	result := BuildTaskContext(run, task, DefaultPolicy())
	designAt := strings.Index(result.Text, "来源任务：design")
	implementAt := strings.Index(result.Text, "来源任务：implement")
	if designAt < 0 || implementAt < 0 || designAt > implementAt {
		t.Fatalf("依赖应按拓扑序（design 在 implement 前）拼装:\n%s", result.Text)
	}
	if len(result.Sources) != 2 || result.Sources[0].TaskID != "design" || result.Sources[1].TaskID != "implement" {
		t.Fatalf("来源顺序错误: %+v", result.Sources)
	}
	// 未完成依赖不注入（防御分支：调度不会放出，但构建函数自身也要稳）。
	blocked := contextRun(
		Task{ID: "design", MemberID: "lead", Ordinal: 0, Status: TaskFailed},
		Task{ID: "review", MemberID: "rev", Ordinal: 1, Prompt: "检查", DependsOn: []string{"design"}, Status: TaskPlanned},
	)
	result = BuildTaskContext(blocked, &blocked.Tasks[1], DefaultPolicy())
	if result.Text != "" {
		t.Fatalf("未完成依赖不应产生上下文，实际 %q", result.Text)
	}
}

func TestBuildTaskContextTruncatesKeepingNewest(t *testing.T) {
	// 预算只够放下最新一块：最旧（design）整块丢弃，标记 Truncated。
	run := contextRun(
		completedDep("design", "lead", 0, strings.Repeat("旧", 200)),
		completedDep("implement", "dev", 1, strings.Repeat("新", 200)),
		Task{ID: "review", MemberID: "rev", Prompt: "检查", DependsOn: []string{"design", "implement"}, Status: TaskPlanned},
	)
	task := &run.Tasks[2]
	policy := DefaultPolicy()
	policy.MaxContextChars = 250 // 来源头约 60 字 + 内容 200 字：只装得下最新一块。
	result := BuildTaskContext(run, task, policy)
	if !result.Truncated {
		t.Fatal("丢弃来源应标记 Truncated")
	}
	if strings.Contains(result.Text, "旧") || !strings.Contains(result.Text, "新") {
		t.Fatalf("截断应优先保留最新结果:\n%s", result.Text)
	}
	if len(result.Sources) != 1 || result.Sources[0].TaskID != "implement" {
		t.Fatalf("只应保留最新来源: %+v", result.Sources)
	}
	if result.InjectedChars > policy.MaxContextChars {
		t.Fatalf("注入长度 %d 超过上限 %d", result.InjectedChars, policy.MaxContextChars)
	}
	// 单块超预算：内容截断但来源保留。
	run = contextRun(
		completedDep("design", "lead", 0, strings.Repeat("果", 300)),
		Task{ID: "impl", MemberID: "dev", Prompt: "实现", DependsOn: []string{"design"}, Status: TaskPlanned},
	)
	policy.MaxContextChars = 150
	result = BuildTaskContext(run, &run.Tasks[1], policy)
	if len(result.Sources) != 1 || !result.Sources[0].Truncated {
		t.Fatalf("单块超限应截断内容并保留来源: %+v", result.Sources)
	}
	if result.InjectedChars > policy.MaxContextChars {
		t.Fatalf("截断后仍超限: %d > %d", result.InjectedChars, policy.MaxContextChars)
	}
}

func TestBuildTaskContextStripsUpstreamEnvelope(t *testing.T) {
	// 上游结果里回显了它自己收到的信封：剥离标记防多轮接力指数增长。
	upstream := `[Team context]
来源成员：lead
来源任务：design
内容可能已截断，完整内容请打开原会话查看。

上一轮的背景说明。

[Assigned task]
分析需求

我的分析结论是……`
	run := contextRun(
		completedDep("design", "lead", 0, upstream),
		Task{ID: "impl", MemberID: "dev", Prompt: "实现", DependsOn: []string{"design"}, Status: TaskPlanned},
	)
	result := BuildTaskContext(run, &run.Tasks[1], DefaultPolicy())
	for _, marker := range []string{"[Team context]", "[Assigned task]", "来源成员：", "来源任务：", contextTruncationNote} {
		// 只允许出现一次（本任务自己的信封头），上游回显的必须被剥掉。
		if strings.Count(result.Text, marker) > 1 {
			t.Fatalf("上游信封标记 %q 未剥离（出现 %d 次）:\n%s",
				marker, strings.Count(result.Text, marker), result.Text)
		}
	}
	if !strings.Contains(result.Text, "我的分析结论是……") {
		t.Fatalf("剥离不应删除正文:\n%s", result.Text)
	}
	// 剥离后同一份结果再嵌一轮也不会增长出第二个信封体。
	nested := ComposeTaskMessage(result.Text, "实现")
	run2 := contextRun(
		completedDep("impl", "dev", 0, nested),
		Task{ID: "review", MemberID: "rev", Prompt: "检查", DependsOn: []string{"impl"}, Status: TaskPlanned},
	)
	again := BuildTaskContext(run2, &run2.Tasks[1], DefaultPolicy())
	if strings.Count(again.Text, "来源任务：impl") != 1 {
		t.Fatalf("多轮注入应只有一个来源块:\n%s", again.Text)
	}
}

func TestBuildTaskMessageByteCapForLedger(t *testing.T) {
	// 账本校验按字节（internal/httpd/queue.go validateSubmissionInput 的
	// len(text) > 100000）；planner 只按 rune 预留余量，多字节内容由
	// BuildTaskContext 的字节兜底保证合成全文可过账。
	policy := DefaultPolicy() // MaxContextChars=12000
	// 场景一：planner 上限内的 ASCII prompt + 满额上下文 → 合成 ≤100000 字节。
	run := contextRun(
		completedDep("design", "lead", 0, strings.Repeat("a", 12000)),
		Task{ID: "impl", MemberID: "dev", Prompt: strings.Repeat("p", 87800), DependsOn: []string{"design"}, Status: TaskPlanned},
	)
	result := BuildTaskContext(run, &run.Tasks[1], policy)
	if text := ComposeTaskMessage(result.Text, run.Tasks[1].Prompt); len(text) > promptHardLimit {
		t.Fatalf("ASCII 满额场景合成 %d 字节，超过账本上限 %d", len(text), promptHardLimit)
	}
	// 场景二：CJK prompt（rune 在 planner 限内、字节超限）→ 上下文按字节收缩。
	run = contextRun(
		completedDep("design", "lead", 0, strings.Repeat("果", 12000)),
		Task{ID: "impl", MemberID: "dev", Prompt: strings.Repeat("字", 5000), DependsOn: []string{"design"}, Status: TaskPlanned},
	)
	result = BuildTaskContext(run, &run.Tasks[1], policy)
	if text := ComposeTaskMessage(result.Text, run.Tasks[1].Prompt); len(text) > promptHardLimit {
		t.Fatalf("CJK 场景合成 %d 字节，超过账本上限 %d", len(text), promptHardLimit)
	}
	if !result.Truncated {
		t.Fatal("字节收缩应标记 Truncated")
	}
	// 场景三：prompt 单独超字节上限（rune 仍在 planner 限内）→ 上下文清零，
	// 由 SubmitTask 的预检把任务确定性拦下。
	run = contextRun(
		completedDep("design", "lead", 0, "分析完成"),
		Task{ID: "impl", MemberID: "dev", Prompt: strings.Repeat("字", 40000), DependsOn: []string{"design"}, Status: TaskPlanned},
	)
	result = BuildTaskContext(run, &run.Tasks[1], policy)
	if result.Text != "" || !result.Truncated {
		t.Fatalf("prompt 单独超限应清空上下文并标记截断: %q truncated=%v", result.Text, result.Truncated)
	}
	if text := ComposeTaskMessage(result.Text, run.Tasks[1].Prompt); len(text) <= promptHardLimit {
		t.Fatalf("该场景合成文本本就超限，测试构造错误: %d 字节", len(text))
	}
}
