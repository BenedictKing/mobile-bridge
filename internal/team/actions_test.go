package team

// actions.go 的单元测试（§13.1 team/actions 行）：标记提取表驱动（不成对/
// 多块/空块/回显吞真块）、严格→宽容两级解码（宽容过仍过同一 schema、宽容
// 不变形合法 JSON）、ValidateActions 全分支（request_context 缺 fromTaskId/
// 引用未完成任务等）、violation 逐条截断、修复链根推导与 ID 边界、ActionKey
// 派生恒等、journal 重建的 action 语义、上下文剥离动作块。执行层（actions-
// exec）：ValidateExecutable 拒绝清单表驱动、派生标识确定性、autoDispatch
// 闸门双态、passActions 直驱（无 goroutine、无固定 sleep）三动作的快照/
// journal/队列断言、拒绝原因落快照、重复幂等键拒绝、§12 验收「两个合法
// delegate 执行后摘要交回 Lead」决策层用例。runner 集成面在 integration-tests-3。

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// actionsRunFixture 构造校验用 Run：lead 已完成（可作为 fromTaskId 来源）、
// dev 计划中（不可作为来源）、含一个既有修复任务（计数用）。
func actionsRunFixture() *Run {
	return &Run{
		ID:     "run-1",
		TeamID: "team-1",
		Status: RunDispatching,
		Round:  1,
		Tasks: []Task{
			{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0},
			{ID: "dev", MemberID: "dev", Status: TaskPlanned, Ordinal: 1},
			{ID: "actfix-lead-1", MemberID: "lead", Status: TaskCompleted, Ordinal: 2},
		},
	}
}

// actionsPolicyFixture 返回 MaxContextChars=99000 的策略：PromptLimit 口径下
// 动作 task 上限压到 800 字，超限用例不必构造 87800 字长串。
func actionsPolicyFixture() TeamPolicy {
	policy := DefaultPolicy()
	policy.MaxContextChars = 99000 // PromptLimit = 100000-99000-200 = 800
	return policy
}

// actionJSON 把动作对象列表拼成信封内容 JSON 文本（严格合法形态）。
func actionJSON(entries ...string) string {
	return `{"actions":[` + strings.Join(entries, ",") + `]}`
}

func delegateEntry(member, task string) string {
	return fmt.Sprintf(`{"type":"delegate","sourceTaskId":"lead","round":1,"member":%q,"task":%q}`, member, task)
}

func TestExtractActionBlockTable(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		wantInner   string
		wantVioSub  string // 期望 violation 包含的子串；空串表示期望无 violation
		wantNoBlock bool
	}{
		{
			name:        "无标记的普通回复",
			text:        "方案已完成，无需进一步动作。",
			wantNoBlock: true,
		},
		{
			name:      "成对标记提取内容",
			text:      "前置说明。\n<team-actions>{\"actions\":[]}</team-actions>\n收尾。",
			wantInner: "{\"actions\":[]}",
		},
		{
			name:       "开始标记未闭合",
			text:       "<team-actions>{\"actions\":[]}",
			wantVioSub: "没有配对的结束标记",
		},
		{
			name:       "孤立结束标记",
			text:       "说明文字</team-actions>",
			wantVioSub: "孤立的结束标记",
		},
		{
			name:       "结束标记先于开始标记",
			text:       "</team-actions><team-actions>",
			wantVioSub: "孤立的结束标记",
		},
		{
			name:       "空块",
			text:       "<team-actions>   </team-actions>",
			wantVioSub: "内容为空",
		},
		{
			name:       "多个块",
			text:       "<team-actions>{}</team-actions> 中间文字 <team-actions>{}</team-actions>",
			wantVioSub: "多个",
		},
		{
			name:       "首个闭合标记后的孤立结束标记也算多块",
			text:       "<team-actions>{}</team-actions> 文字 </team-actions>",
			wantVioSub: "多个",
		},
		{
			name:      "回显引用不吞真实块（取最内层开始标记）",
			text:      "你提到的 <team-actions> 标记要求如下。\n<team-actions>{\"actions\":[]}</team-actions>",
			wantInner: "{\"actions\":[]}",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inner, violations := ExtractActionBlock(testCase.text)
			if testCase.wantNoBlock {
				if inner != "" || len(violations) != 0 {
					t.Fatalf("无标记回复应返回空内容无 violation，实际 inner=%q violations=%v", inner, violations)
				}
				return
			}
			if testCase.wantVioSub != "" {
				if len(violations) == 0 || !strings.Contains(violations[0], testCase.wantVioSub) {
					t.Fatalf("期望 violation 含 %q，实际 %v", testCase.wantVioSub, violations)
				}
				if inner != "" {
					t.Fatalf("结构错误时不应返回内容，实际 %q", inner)
				}
				return
			}
			if len(violations) != 0 {
				t.Fatalf("不应产生 violation，实际 %v", violations)
			}
			if inner != testCase.wantInner {
				t.Fatalf("提取内容不符：want %q got %q", testCase.wantInner, inner)
			}
		})
	}
}

// TestDecodeActionPayloadTwoLevel 覆盖 §5.1 规则 6：严格解码拒绝尾逗号/单引号
// /裸键名；宽容改写后同一出口解码，仍过同一 schema（非法结构照拒）；两级
// 失败汇成同一条 violation。
func TestDecodeActionPayloadTwoLevel(t *testing.T) {
	t.Run("尾逗号经宽容解码通过", func(t *testing.T) {
		payload, violations := decodeActionPayload(`{"actions":[],}`)
		if len(violations) != 0 || payload == nil {
			t.Fatalf("尾逗号应经宽容解码通过，实际 violations=%v", violations)
		}
		if payload.MustGet("actions") == nil {
			t.Fatal("actions 字段应存在")
		}
	})
	t.Run("单引号与裸键名经宽容解码通过", func(t *testing.T) {
		payload, violations := decodeActionPayload(`{'actions':[{type:'delegate'}]}`)
		if len(violations) != 0 || payload == nil {
			t.Fatalf("单引号/裸键应经宽容解码通过，实际 violations=%v", violations)
		}
	})
	t.Run("宽容通过仍过同一 schema（未知成员照拒）", func(t *testing.T) {
		// 尾逗号（actions 数组之后、对象闭合之前）可宽容；但 member 引用不
		// 存在的成员——schema 校验必须照拒（§5.1 规则 6）。
		inner := `{"actions":[` + delegateEntry("nobody", "做事") + `],}`
		payload, violations := decodeActionPayload(inner)
		if len(violations) != 0 {
			t.Fatalf("宽容解码应通过，实际 %v", violations)
		}
		_, schemaViolations := ValidateActions(payload, testTeam(), actionsRunFixture(), DefaultPolicy())
		found := false
		for _, violation := range schemaViolations {
			if strings.Contains(violation, "不存在的成员") {
				found = true
			}
		}
		if !found {
			t.Fatalf("宽容通过后同一 schema 应拒绝未知成员，实际 %v", schemaViolations)
		}
	})
	t.Run("两级都失败汇成一条 violation", func(t *testing.T) {
		_, violations := decodeActionPayload(`{"actions":[}`) // 结构性残缺：宽容也救不了
		if len(violations) != 1 {
			t.Fatalf("应汇成一条 violation，实际 %v", violations)
		}
		if !strings.Contains(violations[0], "严格解析") || !strings.Contains(violations[0], "宽容解析") {
			t.Fatalf("violation 应同时携带两级错误，实际 %q", violations[0])
		}
	})
	t.Run("内容不是单个对象", func(t *testing.T) {
		_, violations := decodeActionPayload(`[1,2]`)
		if len(violations) != 1 || !strings.Contains(violations[0], "单个 JSON 对象") {
			t.Fatalf("数组应被拒绝，实际 %v", violations)
		}
	})
}

// TestLenientJSONPreservesValidJSON 宽容改写不变形合法 JSON：逐字节恒等
// （合法 JSON 不含单引号/裸键/尾逗号，三类变换全不触发）。
func TestLenientJSONPreservesValidJSON(t *testing.T) {
	valid := []string{
		`{}`,
		`[]`,
		`{"actions":[]}`,
		`{"a":1,"b":true,"c":null,"d":"x","e":[1,2,{"f":"g"}]}`,
		"{\n  \"actions\" : [ { \"type\" : \"delegate\" } ]\n}",
		`{"中文":"键值","emoji":"🎉","escape":"a\"b\\c\nd"}`,
		`{"n":1.5,"big":12345678901234567890}`,
		`{"t":true,"f":false}`,
	}
	for _, doc := range valid {
		if _, err := jsoncompat.Unmarshal([]byte(doc)); err != nil {
			t.Fatalf("测试基线必须是合法 JSON：%q err=%v", doc, err)
		}
		if got := lenientJSONText(doc); got != doc {
			t.Fatalf("宽容改写不得变形合法 JSON：\n输入 %q\n输出 %q", doc, got)
		}
	}
}

// TestLenientJSONRewrites 手写变体的改写产物必须能被严格解码接受。
func TestLenientJSONRewrites(t *testing.T) {
	cases := map[string]string{
		`{'a':'b'}`:            `{"a":"b"}`,
		`{a:1, b:2}`:           `{"a":1, "b":2}`,
		`{"list":[1,2,3,]}`:    `{"list":[1,2,3]}`,
		`{"obj":{"x":'y',},}`:  `{"obj":{"x":"y"}}`,
		`{'q':'say "hi"'}`:     `{"q":"say \"hi\""}`,
		`{'esc':'it\'s'}`:      `{"esc":"it's"}`,
		`{'nested':{inner:1}}`: `{"nested":{"inner":1}}`,
	}
	for input, want := range cases {
		got := lenientJSONText(input)
		if _, err := jsoncompat.Unmarshal([]byte(got)); err != nil {
			t.Fatalf("改写产物必须是合法 JSON：%q → %q err=%v", input, got, err)
		}
		// 结构等价（比对解码树，不比对字节——空白保留策略可能不同）。
		gotValue, _ := jsoncompat.Unmarshal([]byte(got))
		wantValue, _ := jsoncompat.Unmarshal([]byte(want))
		if string(jsoncompat.EncodeCompact(gotValue)) != string(jsoncompat.EncodeCompact(wantValue)) {
			t.Fatalf("改写结果结构不符：%q → %q（want %q）", input, got, want)
		}
	}
	// 值位字面量不加引号（true/false/null 不是裸键）。
	if got := lenientJSONText(`{"a":true}`); got != `{"a":true}` {
		t.Fatalf("值位字面量不得加引号：%q", got)
	}
}

// TestValidateActionsTable 覆盖 §5.1 规则 2 与 §5.2 的拒绝清单。
func TestValidateActionsTable(t *testing.T) {
	team := testTeam()
	policy := actionsPolicyFixture()
	run := actionsRunFixture()
	validDelegate := delegateEntry("dev", "实现接口")
	validRequestContext := `{"type":"request_context","sourceTaskId":"lead","round":1,"member":"rev","task":"取回设计","fromTaskId":"lead"}`
	validSummarize := `{"type":"summarize","sourceTaskId":"lead","round":2}`

	cases := []struct {
		name       string
		inner      string
		wantOK     bool
		wantVioSub string
	}{
		{name: "合法 delegate", inner: actionJSON(validDelegate), wantOK: true},
		{name: "合法 request_context 引用已完成任务", inner: actionJSON(validRequestContext), wantOK: true},
		{name: "summarize 无需 member/task/fromTaskId", inner: actionJSON(validSummarize), wantOK: true},
		{name: "空动作数组合法", inner: `{"actions":[]}`, wantOK: true},
		{name: "缺 type", inner: `{"actions":[{"sourceTaskId":"lead","round":1}]}`, wantVioSub: "type 无效"},
		{name: "未知 type", inner: `{"actions":[{"type":"stop","sourceTaskId":"lead","round":1}]}`, wantVioSub: "type 无效"},
		{name: "缺 sourceTaskId", inner: `{"actions":[{"type":"summarize","round":1}]}`, wantVioSub: "缺少 sourceTaskId"},
		{name: "sourceTaskId 不存在", inner: `{"actions":[{"type":"summarize","sourceTaskId":"nobody","round":1}]}`, wantVioSub: "不存在的来源任务"},
		{name: "缺 round", inner: `{"actions":[{"type":"summarize","sourceTaskId":"lead"}]}`, wantVioSub: "缺少 round"},
		{name: "round 非整数", inner: `{"actions":[{"type":"summarize","sourceTaskId":"lead","round":"1"}]}`, wantVioSub: "round 必须是整数"},
		{name: "round 超上限", inner: `{"actions":[{"type":"summarize","sourceTaskId":"lead","round":99}]}`, wantVioSub: "round 超出范围"},
		{name: "round 为 0", inner: `{"actions":[{"type":"summarize","sourceTaskId":"lead","round":0}]}`, wantVioSub: "round 超出范围"},
		{name: "delegate 缺 member", inner: `{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"task":"做事"}]}`, wantVioSub: "缺少 member"},
		{name: "member 不存在", inner: actionJSON(delegateEntry("stranger", "做事")), wantVioSub: "不存在的成员"},
		{name: "member 已停用", inner: actionJSON(delegateEntry("ghost", "做事")), wantVioSub: "已停用的成员"},
		{name: "delegate 缺 task", inner: `{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev"}]}`, wantVioSub: "缺少 task"},
		{name: "task 内容为空白", inner: actionJSON(delegateEntry("dev", "   ")), wantVioSub: "缺少 task"},
		{name: "task 超过 PromptLimit 口径", inner: actionJSON(delegateEntry("dev", strings.Repeat("长", 801))), wantVioSub: "任务内容过长"},
		{name: "request_context 缺 fromTaskId", inner: `{"actions":[{"type":"request_context","sourceTaskId":"lead","round":1,"member":"rev","task":"取回"}]}`, wantVioSub: "缺少 fromTaskId"},
		{name: "request_context 引用未完成任务", inner: `{"actions":[{"type":"request_context","sourceTaskId":"lead","round":1,"member":"rev","task":"取回","fromTaskId":"dev"}]}`, wantVioSub: "尚未完成"},
		{name: "request_context fromTaskId 不存在", inner: `{"actions":[{"type":"request_context","sourceTaskId":"lead","round":1,"member":"rev","task":"取回","fromTaskId":"nobody"}]}`, wantVioSub: "不存在的结果来源任务"},
		{name: "delegate 声明只读 writeLock", inner: `{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"审查实现","writeLock":"none"}]}`, wantOK: true},
		{name: "delegate 声明 exclusive writeLock", inner: `{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"实现接口","writeLock":"exclusive"}]}`, wantOK: true},
		{name: "summarize 声明 writeLock", inner: `{"actions":[{"type":"summarize","sourceTaskId":"lead","round":2,"writeLock":"exclusive"}]}`, wantOK: true},
		{name: "writeLock 取值无效", inner: `{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"做事","writeLock":"shared"}]}`, wantVioSub: "writeLock 取值无效"},
		{name: "request_context 不支持 writeLock", inner: `{"actions":[{"type":"request_context","sourceTaskId":"lead","round":1,"member":"rev","task":"取回","fromTaskId":"lead","writeLock":"none"}]}`, wantVioSub: "不支持 writeLock"},
		{name: "writeLock 非字符串", inner: `{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"做事","writeLock":1}]}`, wantVioSub: "writeLock 必须是字符串"},
		{name: "动作含未识别字段", inner: `{"actions":[{"type":"summarize","sourceTaskId":"lead","round":1,"priority":"high"}]}`, wantVioSub: "无法识别的字段"},
		{name: "顶层含未识别键", inner: `{"actions":[],"note":"x"}`, wantVioSub: "无法识别的字段"},
		{name: "缺 actions 键", inner: `{"tasks":[]}`, wantVioSub: "缺少 actions"},
		{name: "actions 不是数组", inner: `{"actions":{"a":1}}`, wantVioSub: "actions 字段必须是数组"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload, violations := decodeActionPayload(testCase.inner)
			if payload == nil {
				t.Fatalf("基线内文必须可解码，实际 %v", violations)
			}
			actions, violations := ValidateActions(payload, team, run, policy)
			if testCase.wantOK {
				if len(violations) != 0 {
					t.Fatalf("应校验通过，实际 %v", violations)
				}
				// 空动作数组（{"actions":[]}）合法但零产出。
				if len(actions) == 0 && testCase.inner != `{"actions":[]}` {
					t.Fatal("合法动作应产出 Action")
				}
				for _, action := range actions {
					if action.Status != ActionStatusProposed {
						t.Fatalf("动作应落 proposed，实际 %q", action.Status)
					}
					if action.ActionKey == "" {
						t.Fatal("动作必须携带 ActionKey")
					}
				}
				return
			}
			if len(violations) == 0 || !strings.Contains(strings.Join(violations, "；"), testCase.wantVioSub) {
				t.Fatalf("期望 violation 含 %q，实际 %v", testCase.wantVioSub, violations)
			}
			if len(actions) != 0 {
				t.Fatalf("存在 violation 时不得产出动作，实际 %v", actions)
			}
		})
	}
}

// TestValidateActionsCountLimit 动作数上限取 min(MaxTasksPerRun, 8)。
func TestValidateActionsCountLimit(t *testing.T) {
	team := testTeam()
	run := actionsRunFixture()
	policy := DefaultPolicy() // MaxTasksPerRun=12 → 上限 8。
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = delegateEntry("dev", fmt.Sprintf("任务 %d", i))
	}
	payload, _ := decodeActionPayload(actionJSON(nine...))
	_, violations := ValidateActions(payload, team, run, policy)
	found := false
	for _, violation := range violations {
		if strings.Contains(violation, "最多 8 条") {
			found = true
		}
	}
	if !found {
		t.Fatalf("9 条动作应超 8 条上限，实际 %v", violations)
	}
	// MaxTasksPerRun 更小时按策略收口。
	strict := DefaultPolicy()
	strict.MaxTasksPerRun = 3
	four := make([]string, 4)
	for i := range four {
		four[i] = delegateEntry("dev", fmt.Sprintf("任务 %d", i))
	}
	payload, _ = decodeActionPayload(actionJSON(four...))
	_, violations = ValidateActions(payload, team, run, strict)
	found = false
	for _, violation := range violations {
		if strings.Contains(violation, "最多 3 条") {
			found = true
		}
	}
	if !found {
		t.Fatalf("4 条动作应超 MaxTasksPerRun=3 上限，实际 %v", violations)
	}
}

// TestActionKeyDeterministic ActionKey 派生恒等且格式与约定一致。
func TestActionKeyDeterministic(t *testing.T) {
	first := ActionKey("team-1", "run-1", "lead", 1, 0)
	if first != ActionKey("team-1", "run-1", "lead", 1, 0) {
		t.Fatal("同参数派生应恒等")
	}
	if first != "team:team-1:run:run-1:action:lead:1:0" {
		t.Fatalf("ActionKey 格式不符约定: %s", first)
	}
	for _, other := range []string{
		ActionKey("team-2", "run-1", "lead", 1, 0),
		ActionKey("team-1", "run-2", "lead", 1, 0),
		ActionKey("team-1", "run-1", "dev", 1, 0),
		ActionKey("team-1", "run-1", "lead", 2, 0),
		ActionKey("team-1", "run-1", "lead", 1, 1),
	} {
		if other == first {
			t.Fatal("任一参数不同都应派生不同键")
		}
	}
}

// TestRepairPromptBoundedViolations 逐条先截再拼、条数封顶、同输入恒等、
// 不携带字面标记（修复回送文本被引用回显时不吞真实块）。
func TestRepairPromptBoundedViolations(t *testing.T) {
	long := strings.Repeat("错", 500)
	many := make([]string, 25)
	for i := range many {
		many[i] = fmt.Sprintf("问题 %d", i)
	}
	prompt := RepairPrompt([]string{long, "普通问题"}, 2)
	if !strings.Contains(prompt, truncateRunes(long, repairViolationRunes)) {
		t.Fatal("超长 violation 应截到 200 rune 后进入提示词")
	}
	if len([]rune(prompt)) > 8000 {
		t.Fatalf("修复提示词应有界，实际 %d rune", len([]rune(prompt)))
	}
	if !strings.Contains(prompt, "第 2 次解析失败") {
		t.Fatal("提示词应带尝试次数")
	}
	prompt = RepairPrompt(many, 3)
	if !strings.Contains(prompt, fmt.Sprintf(repairOmissionNoteFmt, 5)) {
		t.Fatal("超 20 条应附省略说明（25-20=5）")
	}
	if strings.Count(prompt, "问题 ") != 20 {
		t.Fatalf("应只保留前 20 条，实际 %d", strings.Count(prompt, "问题 "))
	}
	if RepairPrompt(many, 3) != prompt {
		t.Fatal("同输入应恒等（不得掺入 time.Now 等易变字段）")
	}
	if strings.Contains(prompt, actionsOpenTag) || strings.Contains(prompt, actionsCloseTag) {
		t.Fatal("修复提示词不得携带字面标记（回显递归防护）")
	}
}

// TestActionRepairRootAndCounting 修复链根推导（actfix-{root}-{n} 剥回 root；
// 修复任务的动作派生子任务先剥 -{s|d}{round}-{index} 后缀再剥 actfix 前缀，
// 归回同一根链——不剥会把子任务当根、与既有修复任务撞号）与按快照计数
// （只数修复任务本体，派生子任务不得计入）。
func TestActionRepairRootAndCounting(t *testing.T) {
	cases := map[string]string{
		"lead":                    "lead",
		"actfix-lead-1":           "lead",
		"actfix-lead-2":           "lead",
		"actfix-design-2-1":       "design-2",
		"actfix-x":                "x",
		"design-2":                "design-2",
		"lead-s1-2":               "lead",                  // 根任务的派生子任务归回根链
		"lead-d2-0":               "lead",                  // delegate 派生同样归回
		"actfix-lead-1-s1-2":      "lead",                  // 修复任务的派生子任务（复核确认的撞号路径）
		"actfix-lead-1-d1-0":      "lead",                  //
		"actfix-lead-1-s1-0-s1-1": "lead",                  // 嵌套派生逐层剥净
		"actfix-design-2-1-s1-0":  "design-2",              // 带多段根的同款
		"actfix-lead-1-2":         "lead-1",                // 只剥末段修复序号：根的数字末段保留
	}
	for taskID, want := range cases {
		if got := actionRepairRoot(taskID); got != want {
			t.Fatalf("actionRepairRoot(%q)=%q want %q", taskID, got, want)
		}
	}
	run := actionsRunFixture() // 含 actfix-lead-1。
	if got := countActionRepairs(run, "lead"); got != 1 {
		t.Fatalf("应数到 1 个既有修复任务，实际 %d", got)
	}
	// 派生子任务前缀命中但不计入修复回合数（虚高会让修复回合提前 final）。
	derived := actionsRunFixture()
	derived.Tasks = append(derived.Tasks, Task{ID: "actfix-lead-1-s1-2", MemberID: "lead", Status: TaskCompleted, Ordinal: 3})
	if got := countActionRepairs(derived, "lead"); got != 1 {
		t.Fatalf("派生子任务不得计入修复回合数，实际 %d", got)
	}
	// 撞号场景的根推导与下一尝试号：子任务违规 → 根归 lead → 尝试号 2（子任务
	// 自身的 ID 形如 actfix-lead-1-s1-2，绝不与修复任务 actfix-lead-2 撞号）。
	if root := actionRepairRoot("actfix-lead-1-s1-2"); root != "lead" {
		t.Fatalf("子任务应归回根链 lead，实际 %q", root)
	}
	if got := nextActionRepairAttempt(derived, "lead"); got != 2 {
		t.Fatalf("下一尝试号应为 2，实际 %d", got)
	}
	// 撞号守卫：尝试号对应的 ID 已被占用（历史数据）时向后跳到首个空闲号。
	occupied := actionsRunFixture()
	occupied.Tasks = append(occupied.Tasks, Task{ID: "actfix-lead-2", MemberID: "lead", Status: TaskCompleted, Ordinal: 3})
	if got := nextActionRepairAttempt(occupied, "lead"); got != 3 {
		t.Fatalf("被占用的尝试号应跳过，实际 %d", got)
	}
	// 修复回合满 3 后：返回 maxActionRepairs+1（调用方按 final 收场）。
	full := actionsRunFixture()
	full.Tasks = append(full.Tasks,
		Task{ID: "actfix-lead-2", MemberID: "lead", Status: TaskCompleted, Ordinal: 3},
		Task{ID: "actfix-lead-3", MemberID: "lead", Status: TaskCompleted, Ordinal: 4})
	if got := nextActionRepairAttempt(full, "lead"); got != maxActionRepairs+1 {
		t.Fatalf("满 3 回合应返回 %d（final），实际 %d", maxActionRepairs+1, got)
	}
	// ID 边界：源任务 ID 60 字符时 actfix-{src}-1 全长 68 超过 64 —— 修复通道
	// 必须确定性收场（ValidMemberID 拒绝）。
	longSource := strings.Repeat("a", 60)
	if !ValidMemberID(longSource) {
		t.Fatal("基线：60 字符源任务 ID 本身合法")
	}
	if ValidMemberID(fmt.Sprintf("%s%s-1", actionRepairPrefix, longSource)) {
		t.Fatal("actfix-{60 字符源}-1 超长必须被成员 ID 正则拒绝")
	}
}

// TestRebuildActionEvents journal 重建的 action 语义：action_proposed 追加
// proposed 动作并推进轮次；action_rejected 置拒绝；携带 actionKey 的
// task_planned/task_forwarded 置 executed；invalid_action_block 与修复
// task_planned 的 round 增载推进 Run.Round。
func TestRebuildActionEvents(t *testing.T) {
	run := &Run{ID: "run-9", TeamID: "team-1", Status: RunDispatching, Round: 1}
	apply := func(eventType string, payload map[string]any) {
		applyJournalEventToRun(run, &Event{
			RunID:   run.ID,
			Type:    eventType,
			Payload: payload,
		})
	}
	apply(EventTaskPlanned, map[string]any{
		"taskId": "lead", "memberId": "lead", "instructionsHead": "目标",
	})
	apply(EventTaskPlanned, map[string]any{
		"taskId": "dev", "memberId": "dev", "instructionsHead": "实现",
	})
	apply(EventActionProposed, map[string]any{
		"taskId": "lead", "index": jsoncompat.Num(0), "actionKey": "key-a",
		"type": "delegate", "round": jsoncompat.Num(2),
		"sourceTaskId": "lead", "member": "dev", "taskHead": "实现接口",
	})
	if len(run.PendingActions) != 1 {
		t.Fatalf("action_proposed 应重建动作，实际 %v", run.PendingActions)
	}
	action := run.PendingActions[0]
	if action.ActionKey != "key-a" || action.Status != ActionStatusProposed ||
		action.Type != ActionTypeDelegate || action.Member != "dev" ||
		action.Task != "实现接口" || action.Round != 2 || action.SourceTaskID != "lead" {
		t.Fatalf("重建动作字段不符: %+v", action)
	}
	if run.Round != 2 {
		t.Fatalf("action_proposed 的 round 应推进 Run.Round，实际 %d", run.Round)
	}
	// 第二条动作经执行流落地（task_planned 携带 actionKey）→ executed。
	apply(EventActionProposed, map[string]any{
		"taskId": "lead", "index": jsoncompat.Num(1), "actionKey": "key-b",
		"type": "summarize", "round": jsoncompat.Num(2), "sourceTaskId": "lead",
	})
	apply(EventTaskPlanned, map[string]any{
		"taskId": "sum-1", "memberId": "lead", "actionKey": "key-b",
	})
	if run.PendingActions[1].Status != ActionStatusExecuted {
		t.Fatalf("携带 actionKey 的 task_planned 应置 executed，实际 %q", run.PendingActions[1].Status)
	}
	// 拒绝是终局：rejected 不再被 executed 改写。
	apply(EventActionRejected, map[string]any{"actionKey": "key-a", "reason": "超出并发预算"})
	if run.PendingActions[0].Status != ActionStatusRejected || run.PendingActions[0].Reason != "超出并发预算" {
		t.Fatalf("action_rejected 应置 rejected+Reason，实际 %+v", run.PendingActions[0])
	}
	apply(EventTaskPlanned, map[string]any{
		"taskId": "del-1", "memberId": "dev", "actionKey": "key-a",
	})
	if run.PendingActions[0].Status != ActionStatusRejected {
		t.Fatalf("rejected 是终局，不得回写 executed，实际 %q", run.PendingActions[0].Status)
	}
	// task_forwarded 携带 actionKey 同样是执行凭据。
	apply(EventActionProposed, map[string]any{
		"taskId": "lead", "index": jsoncompat.Num(2), "actionKey": "key-c",
		"type": "request_context", "round": jsoncompat.Num(2), "sourceTaskId": "lead",
	})
	apply(EventTaskForwarded, map[string]any{
		"targetMemberId": "rev", "actionKey": "key-c",
	})
	if run.PendingActions[len(run.PendingActions)-1].Status != ActionStatusExecuted {
		t.Fatal("携带 actionKey 的 task_forwarded 应置 executed")
	}
	// invalid_action_block 与修复 task_planned 的 round 增载。
	apply(EventInvalidActionBlock, map[string]any{
		"taskId": "lead", "attempt": jsoncompat.Num(1), "final": false,
		"round": jsoncompat.Num(3), "violations": []any{"问题一"},
	})
	if run.Round != 3 {
		t.Fatalf("invalid_action_block 的 round 应推进 Run.Round，实际 %d", run.Round)
	}
	apply(EventTaskPlanned, map[string]any{
		"taskId": "actfix-lead-1", "memberId": "lead", "round": jsoncompat.Num(3),
	})
	if repaired := findTaskIn(run, "actfix-lead-1"); repaired == nil || repaired.Status != TaskPlanned {
		t.Fatalf("修复任务的 task_planned 应重建任务，实际 %+v", repaired)
	}
	if run.Round != 3 {
		t.Fatalf("修复 task_planned 不得回退轮次，实际 %d", run.Round)
	}
	// 缺省 round（0）不动。
	apply(EventTaskPlanned, map[string]any{"taskId": "plain-1", "memberId": "dev"})
	if run.Round != 3 {
		t.Fatalf("无 round 的 task_planned 不应推进轮次，实际 %d", run.Round)
	}
}

// TestStripActionBlocksAndContextInjection 上下文注入前剥离动作块：Lead 结果
// 里携带的 <team-actions> 绝不进入下游 [Team context]。
func TestStripActionBlocksAndContextInjection(t *testing.T) {
	if got := stripActionBlocks("a<team-actions>{}</team-actions>b"); got != "ab" {
		t.Fatalf("单个块应被剥除：%q", got)
	}
	if got := stripActionBlocks("a<team-actions>1</team-actions>b<team-actions>2</team-actions>c"); got != "abc" {
		t.Fatalf("多个块应循环剥除：%q", got)
	}
	if got := stripActionBlocks("孤立<team-actions>未闭合"); got != "孤立<team-actions>未闭合" {
		t.Fatalf("不成对标记原样保留：%q", got)
	}
	// 经 BuildTaskContext 的完整路径。
	team := testTeam()
	run := actionsRunFixture()
	downstream := &Task{ID: "dev", MemberID: "dev", Prompt: "继续实现", DependsOn: []string{"lead"}}
	leadResult := "设计结论……\n<team-actions>{\"actions\":[]}</team-actions>"
	run.Tasks[0].Status = TaskCompleted
	run.Tasks[0].Result = leadResult
	run.Tasks[0].ResultDetail = &TaskResult{Result: leadResult}
	context := BuildTaskContext(run, downstream, team.Policy)
	if strings.Contains(context.Text, actionsOpenTag) || strings.Contains(context.Text, actionsCloseTag) {
		t.Fatalf("注入上下文不得包含动作块标记：%q", context.Text)
	}
	if !strings.Contains(context.Text, "设计结论") {
		t.Fatalf("正文应保留：%q", context.Text)
	}
}

// TestParseLeadActionsOrdinaryReply 普通文本回复（无标记）不产 violation 也不
// 产动作——自然语言绝不触发动作（§5.1 开篇的安全边界）。
func TestParseLeadActionsOrdinaryReply(t *testing.T) {
	actions, violations := ParseLeadActions("请让 reviewer 看一下这份实现。", testTeam(), actionsRunFixture(), DefaultPolicy())
	if len(violations) != 0 || len(actions) != 0 {
		t.Fatalf("普通文本不得触发动作或修复，实际 actions=%v violations=%v", actions, violations)
	}
}

// TestAutoPlanAndActionsPersistRoundTrip autoPlan 与扩展 Action 字段经
// JSON 账本（team-runs.json）无损往返：漏 runKnownFields/jsonObject 键会让
// 重启后解析闸门静默关闭或动作事实丢失。
func TestAutoPlanAndActionsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(dir+"/teams.json", dir+"/team-runs.json")
	team := testTeam()
	if err := store.SaveTeam(team); err != nil {
		t.Fatal(err)
	}
	run := &Run{
		ID: "run-rt", TeamID: team.ID, Goal: "目标", Mode: ModeSingle,
		AutoPlan: true, Status: RunDispatching, Round: 2, PlanHash: "ph",
		Tasks: []Task{{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0}},
		PendingActions: []Action{{
			Type: ActionTypeRequestContext, SourceTaskID: "lead", Round: 2,
			Member: "rev", Task: "取回设计", FromTaskID: "lead",
			ActionKey: "key-rt", Status: ActionStatusProposed,
		}},
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	fresh := LoadStore(dir+"/teams.json", dir+"/team-runs.json").Run("run-rt")
	if fresh == nil || !fresh.AutoPlan {
		t.Fatalf("autoPlan 必须随快照持久化: %+v", fresh)
	}
	if len(fresh.PendingActions) != 1 {
		t.Fatalf("动作必须随快照持久化: %+v", fresh.PendingActions)
	}
	action := fresh.PendingActions[0]
	if action.FromTaskID != "lead" || action.ActionKey != "key-rt" ||
		action.Status != ActionStatusProposed || action.Task != "取回设计" {
		t.Fatalf("动作扩展字段往返不符: %+v", action)
	}
}

// ---- 执行层（§5.2 拒绝清单 + passActions 直驱）----

// actionsExecHarness 是执行层测试基座：直调 passActions（不启动循环/Watcher
// goroutine），快照/journal/队列断言全部同步完成——状态机正确性不靠固定
// sleep 判定（§13.1）。
type actionsExecHarness struct {
	store    *memStore
	journal  *Journal
	queue    *fakeQueue
	resolver *fakeResolver
	service  *Service
	team     *Team
}

func newActionsExecHarness(t *testing.T) *actionsExecHarness {
	t.Helper()
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "team-journal.jsonl"))
	if err != nil {
		t.Fatalf("journal 打开失败: %v", err)
	}
	h := &actionsExecHarness{
		store:    newMemStore(),
		journal:  journal,
		queue:    newFakeQueue(),
		resolver: newFakeResolver(),
	}
	h.team = testTeam()
	// 执行层基线：闸门开（DefaultPolicy 关闭 autoDispatch——闸门双态由
	// TestPassActionsAutoDispatchGate 显式覆盖）。
	h.team.Policy.AutoDispatch = true
	for _, member := range h.team.Members {
		h.resolver.setSession(member.SessionID, newFakeSession(member.SessionID, "/tmp/"+member.ID))
		h.resolver.setCwd(member.SessionID, "/tmp/"+member.ID)
	}
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	h.service = NewService(h.store, h.journal, h.queue, h.resolver)
	return h
}

// saveExecRun 落快照并返回执行状态（stateFor 不启动循环）。
func (h *actionsExecHarness) saveExecRun(t *testing.T, run *Run) *runState {
	t.Helper()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
	return h.service.stateFor(run.ID)
}

// eventsOf 重放 journal 并按类型过滤本 Run 事件。
func (h *actionsExecHarness) eventsOf(t *testing.T, eventType string) []*Event {
	t.Helper()
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	var matched []*Event
	for _, event := range events {
		if event != nil && event.RunID == "run-ex" && event.Type == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// execRunFixture 构造执行用 Run：lead 任务已完成（动作来源），动作列表可变。
func execRunFixture(actions ...Action) *Run {
	return &Run{
		ID: "run-ex", TeamID: "team-1", Goal: "实现用户头像上传接口", Mode: ModeSingle,
		AutoPlan: true, Status: RunDispatching, Round: 1,
		Tasks: []Task{{
			ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0,
			Result: "Lead 阶段结论", ResultDetail: &TaskResult{Result: "Lead 阶段结论"},
		}},
		PendingActions: actions,
	}
}

// TestValidateExecutableTable 覆盖 §5.2 拒绝清单逐条（纯决策层）。
func TestValidateExecutableTable(t *testing.T) {
	team := testTeam()
	policy := DefaultPolicy()
	baseRun := func() *Run {
		return &Run{
			ID: "run-1", TeamID: "team-1", Status: RunDispatching, Round: 1,
			Tasks: []Task{{
				ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0,
				Result: "结论", ResultDetail: &TaskResult{Result: "结论"},
			}},
		}
	}
	delegate := func(mutate func(*Action)) *Action {
		action := &Action{
			Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
			Member: "dev", Task: "实现接口",
			ActionKey: ActionKey("team-1", "run-1", "lead", 1, 0),
		}
		if mutate != nil {
			mutate(action)
		}
		return action
	}
	requestContext := func(mutate func(*Action)) *Action {
		action := &Action{
			Type: ActionTypeRequestContext, SourceTaskID: "lead", Round: 1,
			Member: "rev", Task: "取回设计", FromTaskID: "lead",
			ActionKey: ActionKey("team-1", "run-1", "lead", 1, 1),
		}
		if mutate != nil {
			mutate(action)
		}
		return action
	}
	cases := []struct {
		name    string
		action  *Action
		pre     *actionPrefetch
		busy    map[string]bool
		wantOK  bool
		wantSub string
	}{
		{name: "合法 delegate", action: delegate(nil), wantOK: true},
		{name: "合法 request_context", action: requestContext(nil), wantOK: true},
		{name: "合法 summarize（目标 Lead）", action: &Action{
			Type: ActionTypeSummarize, SourceTaskID: "lead", Round: 2,
			ActionKey: ActionKey("team-1", "run-1", "lead", 2, 0),
		}, wantOK: true},
		{name: "未知动作类型", action: delegate(func(a *Action) { a.Type = "stop" }), wantSub: "未知动作类型"},
		{name: "轮次超上限", action: delegate(func(a *Action) { a.Round = 99 }), wantSub: "轮次超出范围"},
		{name: "来源任务不存在", action: delegate(func(a *Action) {
			a.SourceTaskID = "nobody"
			a.ActionKey = ActionKey("team-1", "run-1", "nobody", 1, 0)
		}), wantSub: "不存在的来源任务"},
		{name: "成员不存在", action: delegate(func(a *Action) { a.Member = "stranger" }), wantSub: "不存在的成员"},
		{name: "成员已停用", action: delegate(func(a *Action) { a.Member = "ghost" }), wantSub: "已停用的成员"},
		{name: "成员会话失效", action: delegate(nil), pre: &actionPrefetch{
			sessionLive: map[string]bool{"dev": false},
		}, wantSub: "绑定会话已失效"},
		{name: "request_context 缺 fromTaskId", action: requestContext(func(a *Action) { a.FromTaskID = "" }), wantSub: "缺少结果来源任务"},
		{name: "request_context 来源未完成", action: requestContext(func(a *Action) { a.FromTaskID = "wip" }), wantSub: "尚未完成"},
		{name: "request_context 来源无内容", action: requestContext(func(a *Action) { a.FromTaskID = "empty" }), wantSub: "没有可转发的内容"},
		{name: "任务内容超上限", action: delegate(func(a *Action) { a.Task = strings.Repeat("长", 801) }), pre: &actionPrefetch{sessionLive: map[string]bool{"dev": true}}, wantSub: "任务内容过长"},
		{name: "派生任务 ID 超长", action: delegate(func(a *Action) {
			a.SourceTaskID = strings.Repeat("a", 60)
			a.ActionKey = ActionKey("team-1", "run-1", strings.Repeat("a", 60), 1, 0)
		}), wantSub: "派生任务 ID 超长"},
		{name: "派生任务 ID 已存在", action: delegate(nil), wantSub: "派生任务 ID 已存在"},
		{name: "任务数达上限", action: delegate(nil), wantSub: "任务数已达上限"},
		{name: "成员已有运行任务", action: delegate(nil), busy: map[string]bool{"dev": true}, wantSub: "已有运行中的任务"},
		{name: "团队并发已满", action: delegate(nil), pre: &actionPrefetch{
			sessionLive: map[string]bool{"dev": true}, teamInFlight: 3,
		}, wantSub: "团队并发已满"},
		{name: "同键快照重复提案", action: delegate(nil), wantSub: "重复的动作幂等键"},
		{name: "同键 journal 重复提案", action: delegate(nil), pre: &actionPrefetch{
			sessionLive: map[string]bool{"dev": true},
			facts: &actionJournalFacts{proposed: map[string]int{
				ActionKey("team-1", "run-1", "lead", 1, 0): 2,
			}},
		}, wantSub: "重复的动作幂等键"},
		{name: "动作幂等键无序号", action: delegate(func(a *Action) { a.ActionKey = "broken-key" }), wantSub: "无法派生任务 ID"},
		{name: "summarize 团队无 Lead", action: &Action{
			Type: ActionTypeSummarize, SourceTaskID: "lead", Round: 1,
			ActionKey: ActionKey("team-1", "run-1", "lead", 1, 2),
		}, wantSub: "未指定 Lead"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := baseRun()
			// 各用例的快照前置：wip=计划中（未完成）、empty=已完成但无结果、
			// dup=同键既有动作条目、capped=任务数达 MaxTasksPerRun。
			run.Tasks = append(run.Tasks,
				Task{ID: "wip", MemberID: "rev", Status: TaskPlanned, Ordinal: 1},
				Task{ID: "empty", MemberID: "rev", Status: TaskCompleted, Ordinal: 2},
			)
			switch testCase.name {
			case "派生任务 ID 超长":
				// 60 字符来源任务在库且已完成：只有派生 ID 超限这一条拒绝理由。
				run.Tasks = append(run.Tasks, Task{
					ID: strings.Repeat("a", 60), MemberID: "lead", Status: TaskCompleted, Ordinal: 3,
				})
			case "派生任务 ID 已存在":
				run.Tasks = append(run.Tasks, Task{ID: "lead-d1-0", MemberID: "dev", Status: TaskPlanned, Ordinal: 3})
			case "任务内容超上限":
				// MaxContextChars=99000 → PromptLimit=800（actionsPolicyFixture 口径）。
				ok, reason := ValidateExecutable(run, team, actionsPolicyFixture(), testCase.action, testCase.pre, testCase.busy)
				if ok || !strings.Contains(reason, testCase.wantSub) {
					t.Fatalf("期望拒绝（%s），实际 ok=%v reason=%q", testCase.wantSub, ok, reason)
				}
				return
			case "任务数达上限":
				strict := DefaultPolicy()
				strict.MaxTasksPerRun = 3 // lead+wip+empty 已满。
				ok, reason := ValidateExecutable(run, team, strict, testCase.action, testCase.pre, testCase.busy)
				if ok || !strings.Contains(reason, testCase.wantSub) {
					t.Fatalf("期望拒绝（%s），实际 ok=%v reason=%q", testCase.wantSub, ok, reason)
				}
				return
			case "同键快照重复提案":
				// 真实流里被判定动作本身就在快照中（passActions 从 PendingActions
				// 读出）：动作 + 同键副本两条同存才是重复提案。
				run.PendingActions = append(run.PendingActions, *testCase.action, Action{
					ActionKey: testCase.action.ActionKey,
					Status:    ActionStatusProposed,
				})
			case "summarize 团队无 Lead":
				noLead := testTeam()
				noLead.LeadID = ""
				ok, reason := ValidateExecutable(run, noLead, policy, testCase.action, testCase.pre, testCase.busy)
				if ok || !strings.Contains(reason, testCase.wantSub) {
					t.Fatalf("期望拒绝（%s），实际 ok=%v reason=%q", testCase.wantSub, ok, reason)
				}
				return
			}
			ok, reason := ValidateExecutable(run, team, policy, testCase.action, testCase.pre, testCase.busy)
			if testCase.wantOK {
				if !ok {
					t.Fatalf("应校验通过，实际拒绝：%s", reason)
				}
				return
			}
			if ok || !strings.Contains(reason, testCase.wantSub) {
				t.Fatalf("期望拒绝（%s），实际 ok=%v reason=%q", testCase.wantSub, ok, reason)
			}
		})
	}
}

// TestActionDerivedIdentifiers 派生任务 ID / 转发请求 ID / 序号解析的确定性。
func TestActionDerivedIdentifiers(t *testing.T) {
	delegate := &Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 2, Task: "实现接口",
		ActionKey: ActionKey("team-1", "run-1", "lead", 2, 3),
	}
	summarize := &Action{
		Type: ActionTypeSummarize, SourceTaskID: "lead", Round: 2,
		ActionKey: ActionKey("team-1", "run-1", "lead", 2, 3),
	}
	if got := ActionDerivedTaskID(delegate); got != "lead-d2-3" {
		t.Fatalf("delegate 派生 ID 不符: %s", got)
	}
	if got := ActionDerivedTaskID(summarize); got != "lead-s2-3" {
		t.Fatalf("summarize 派生 ID 不符: %s", got)
	}
	// 同源同轮靠 index 唯一（评审问题⑤⑥）。
	first := &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, ActionKey: ActionKey("t", "r", "lead", 1, 0)}
	second := &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, ActionKey: ActionKey("t", "r", "lead", 1, 1)}
	if ActionDerivedTaskID(first) == ActionDerivedTaskID(second) {
		t.Fatal("同源同轮不同 index 的派生 ID 必须不同")
	}
	// 键形态损坏（无序号）确定性拒绝。
	for _, broken := range []string{"", "no-separator", "team:t:run:r:action:lead:1:x"} {
		if id := ActionDerivedTaskID(&Action{Type: ActionTypeDelegate, SourceTaskID: "lead", ActionKey: broken}); id != "" {
			t.Fatalf("损坏键 %q 不应派生 ID，实际 %q", broken, id)
		}
	}
	// RequestID 确定性 + UUID 形态。
	key := ActionKey("team-1", "run-1", "lead", 1, 0)
	if ActionRequestID(key) != ActionRequestID(key) {
		t.Fatal("同键派生 RequestID 应恒等")
	}
	if len(ActionRequestID(key)) != 36 || strings.Count(ActionRequestID(key), "-") != 4 {
		t.Fatalf("RequestID 应是 UUID 形态: %s", ActionRequestID(key))
	}
	if ActionRequestID(key) == ActionRequestID(ActionKey("team-1", "run-1", "lead", 1, 1)) {
		t.Fatal("不同动作必须派生不同 RequestID")
	}
	// summarize 缺省 prompt 确定性。
	if got := ActionTaskPrompt(&Action{Type: ActionTypeSummarize}); got != summarizePromptDefault {
		t.Fatalf("summarize 缺省 prompt 不符: %q", got)
	}
	if got := ActionTaskPrompt(delegate); got != "实现接口" {
		t.Fatalf("携带 task 的动作应以 task 为 prompt: %q", got)
	}
}

// TestHasRunnableProposedActions fold 拦截条件与 autoDispatch 闸门双态一致。
func TestHasRunnableProposedActions(t *testing.T) {
	policyOn := DefaultPolicy()
	policyOn.AutoDispatch = true
	policyOff := DefaultPolicy()
	policyOff.AutoDispatch = false
	delegate := Action{Type: ActionTypeDelegate, ActionKey: "k-d", Status: ActionStatusProposed}
	requestContext := Action{Type: ActionTypeRequestContext, ActionKey: "k-r", Status: ActionStatusProposed}
	summarize := Action{Type: ActionTypeSummarize, ActionKey: "k-s", Status: ActionStatusProposed}
	if hasRunnableProposedActions(nil, policyOn) {
		t.Fatal("nil run 无待执行动作")
	}
	if hasRunnableProposedActions(&Run{}, policyOn) {
		t.Fatal("无动作的 run 不拦 fold")
	}
	// 闸门开：三类都拦；闸门关：仅 summarize 拦（delegate/rc 停 proposed 不执行，
	// 不构成 fold 阻塞——否则关闭 autoDispatch 的 Run 永远无法终局）。
	for _, action := range []Action{delegate, requestContext, summarize} {
		run := &Run{PendingActions: []Action{action}}
		if !hasRunnableProposedActions(run, policyOn) {
			t.Fatalf("闸门开时 %s 应拦 fold", action.Type)
		}
	}
	for _, action := range []Action{delegate, requestContext} {
		run := &Run{PendingActions: []Action{action}}
		if hasRunnableProposedActions(run, policyOff) {
			t.Fatalf("闸门关时 %s 不应拦 fold", action.Type)
		}
	}
	if !hasRunnableProposedActions(&Run{PendingActions: []Action{summarize}}, policyOff) {
		t.Fatal("summarize 恒可执行，闸门关也应拦 fold")
	}
	// 非 proposed 状态（预览已落定）不拦。
	settled := &Run{PendingActions: []Action{
		{Type: ActionTypeDelegate, ActionKey: "k", Status: ActionStatusExecuted},
		{Type: ActionTypeSummarize, ActionKey: "k2", Status: ActionStatusRejected},
	}}
	if hasRunnableProposedActions(settled, policyOn) {
		t.Fatal("executed/rejected 动作不拦 fold")
	}
}

// TestPassActionsExecutesDelegateAndAdvancesRound delegate 执行：派生任务入
// 快照、task_planned 事件增载 round+actionKey、动作 executed、Run.Round 推进。
func TestPassActionsExecutesDelegateAndAdvancesRound(t *testing.T) {
	h := newActionsExecHarness(t)
	key := ActionKey("team-1", "run-ex", "lead", 2, 0)
	run := execRunFixture(Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 2,
		Member: "dev", Task: "实现接口",
		ActionKey: key, Status: ActionStatusProposed,
	})
	state := h.saveExecRun(t, run)
	h.service.passActions(state)
	fresh := h.store.Run("run-ex")
	if len(fresh.PendingActions) != 1 || fresh.PendingActions[0].Status != ActionStatusExecuted {
		t.Fatalf("动作应落 executed: %+v", fresh.PendingActions)
	}
	derived := findTaskIn(fresh, "lead-d2-0")
	if derived == nil {
		t.Fatalf("应创建派生任务 lead-d2-0: %+v", fresh.Tasks)
	}
	if derived.MemberID != "dev" || derived.Prompt != "实现接口" ||
		derived.Status != TaskPlanned || derived.WriteLock != TaskWriteLockExclusive ||
		derived.WorkspaceMode != WorkspaceShared || derived.InputHash == "" {
		t.Fatalf("派生任务字段不符: %+v", derived)
	}
	if derived.DependsOn != nil {
		t.Fatalf("无 fromTaskId 的 delegate 不应有依赖: %+v", derived.DependsOn)
	}
	if fresh.Round != 2 {
		t.Fatalf("Run.Round 应推进到动作轮次 2，实际 %d", fresh.Round)
	}
	planned := h.eventsOf(t, EventTaskPlanned)
	if len(planned) != 1 {
		t.Fatalf("应恰有一条 task_planned，实际 %d", len(planned))
	}
	event := planned[0]
	if payloadString(event.Payload, "taskId") != "lead-d2-0" ||
		payloadString(event.Payload, "actionKey") != key ||
		payloadInt(event.Payload, "round") != 2 ||
		payloadString(event.Payload, "memberId") != "dev" {
		t.Fatalf("task_planned 载荷不符: %+v", event.Payload)
	}
	if event.IdempotencyKey == "" {
		t.Fatal("task_planned 应携带幂等键（崩溃窗口防重）")
	}
}

// TestPassActionsSummarizeReturnsCompletedResults §12 阶段三验收（决策层）：
// 两个合法 delegate 执行并完成后，summarize 派生任务的依赖=全部已完成任务集，
// BuildTaskContext 把两份 delegate 结果摘要注入 Lead 的上下文——「摘要交回
// Lead」。
func TestPassActionsSummarizeReturnsCompletedResults(t *testing.T) {
	h := newActionsExecHarness(t)
	run := execRunFixture(Action{
		Type: ActionTypeSummarize, SourceTaskID: "lead", Round: 2,
		ActionKey: ActionKey("team-1", "run-ex", "lead", 2, 0), Status: ActionStatusProposed,
	})
	// 两个 delegate 派生任务已执行并完成（结果在库）。
	run.Tasks = append(run.Tasks,
		Task{ID: "lead-d1-0", MemberID: "dev", Status: TaskCompleted, Ordinal: 1,
			Result: "接口实现完成", ResultDetail: &TaskResult{Result: "接口实现完成"}},
		Task{ID: "lead-d1-1", MemberID: "rev", Status: TaskCompleted, Ordinal: 2,
			Result: "回归测试通过", ResultDetail: &TaskResult{Result: "回归测试通过"}},
	)
	state := h.saveExecRun(t, run)
	h.service.passActions(state)
	fresh := h.store.Run("run-ex")
	if fresh.PendingActions[0].Status != ActionStatusExecuted {
		t.Fatalf("summarize 应落 executed: %+v", fresh.PendingActions)
	}
	derived := findTaskIn(fresh, "lead-s2-0")
	if derived == nil {
		t.Fatalf("应创建 summarize 派生任务 lead-s2-0: %+v", fresh.Tasks)
	}
	if derived.MemberID != "lead" {
		t.Fatalf("summarize 派生任务应交给 Lead: %+v", derived)
	}
	deps := map[string]bool{}
	for _, dep := range derived.DependsOn {
		deps[dep] = true
	}
	for _, want := range []string{"lead", "lead-d1-0", "lead-d1-1"} {
		if !deps[want] {
			t.Fatalf("summarize 依赖应含全部已完成任务 %s: %+v", want, derived.DependsOn)
		}
	}
	if derived.Prompt != summarizePromptDefault {
		t.Fatalf("未携带 task 的 summarize 应用缺省摘要指令: %q", derived.Prompt)
	}
	// 摘要交回 Lead：派发文本注入两份 delegate 结果。
	context := BuildTaskContext(fresh, derived, h.team.Policy)
	sentText := ComposeTaskMessage(context.Text, derived.Prompt)
	for _, want := range []string{"接口实现完成", "回归测试通过", summarizePromptDefault} {
		if !strings.Contains(sentText, want) {
			t.Fatalf("summarize 派发文本应含 %q:\n%s", want, sentText)
		}
	}
}

// TestPassActionsRejectionLandsInSnapshot 拒绝原因落快照（唯一持久落点）+
// action_rejected 事件（IdempotencyKey=ActionKey，payload 五字段）。
func TestPassActionsRejectionLandsInSnapshot(t *testing.T) {
	h := newActionsExecHarness(t)
	key := ActionKey("team-1", "run-ex", "lead", 1, 0)
	run := execRunFixture(Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
		Member: "stranger", Task: "实现接口", // 成员不存在：确定性拒绝。
		ActionKey: key, Status: ActionStatusProposed,
	})
	state := h.saveExecRun(t, run)
	h.service.passActions(state)
	fresh := h.store.Run("run-ex")
	if len(fresh.Tasks) != 1 {
		t.Fatalf("被拒动作不得创建任务: %+v", fresh.Tasks)
	}
	action := fresh.PendingActions[0]
	if action.Status != ActionStatusRejected || !strings.Contains(action.Reason, "不存在的成员") {
		t.Fatalf("拒绝原因应落快照字段: %+v", action)
	}
	rejected := h.eventsOf(t, EventActionRejected)
	if len(rejected) != 1 {
		t.Fatalf("应恰有一条 action_rejected，实际 %d", len(rejected))
	}
	event := rejected[0]
	if event.IdempotencyKey != key {
		t.Fatalf("action_rejected 幂等键应为 ActionKey: %q", event.IdempotencyKey)
	}
	if payloadString(event.Payload, "taskId") != "lead" ||
		payloadString(event.Payload, "type") != ActionTypeDelegate ||
		payloadString(event.Payload, "actionKey") != key ||
		payloadInt(event.Payload, "index") != 0 ||
		!strings.Contains(payloadString(event.Payload, "reason"), "不存在的成员") {
		t.Fatalf("action_rejected 载荷不符: %+v", event.Payload)
	}
}

// TestPassActionsAutoDispatchGate 执行闸门双态：false 时 delegate/request_
// context 停在 proposed（免执行预览，不落账不转发），summarize 照常执行。
func TestPassActionsAutoDispatchGate(t *testing.T) {
	h := newActionsExecHarness(t)
	h.team.Policy.AutoDispatch = false
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	run := execRunFixture(
		Action{
			Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
			Member: "dev", Task: "实现接口",
			ActionKey: ActionKey("team-1", "run-ex", "lead", 1, 0), Status: ActionStatusProposed,
		},
		Action{
			Type: ActionTypeRequestContext, SourceTaskID: "lead", Round: 1,
			Member: "rev", Task: "取回设计", FromTaskID: "lead",
			ActionKey: ActionKey("team-1", "run-ex", "lead", 1, 1), Status: ActionStatusProposed,
		},
		Action{
			Type: ActionTypeSummarize, SourceTaskID: "lead", Round: 1,
			ActionKey: ActionKey("team-1", "run-ex", "lead", 1, 2), Status: ActionStatusProposed,
		},
	)
	state := h.saveExecRun(t, run)
	h.service.passActions(state)
	fresh := h.store.Run("run-ex")
	for i, want := range []string{ActionStatusProposed, ActionStatusProposed, ActionStatusExecuted} {
		if fresh.PendingActions[i].Status != want {
			t.Fatalf("第 %d 个动作状态应为 %s，实际 %s（%+v）", i, want, fresh.PendingActions[i].Status, fresh.PendingActions[i])
		}
	}
	if findTaskIn(fresh, "lead-d1-0") != nil {
		t.Fatal("闸门关闭时 delegate 不得创建任务")
	}
	if entries := h.queue.entriesFor("s-dev"); len(entries) != 0 {
		t.Fatalf("闸门关闭时不得入队: %+v", entries)
	}
	if entries := h.queue.entriesFor("s-rev"); len(entries) != 0 {
		t.Fatalf("闸门关闭时不得转发: %+v", entries)
	}
	if len(h.eventsOf(t, EventActionRejected)) != 0 {
		t.Fatal("闸门关闭是免执行预览，不是拒绝")
	}
	if summarize := findTaskIn(fresh, "lead-s1-2"); summarize == nil {
		t.Fatalf("summarize 不受闸门约束: %+v", fresh.Tasks)
	}
	// 闸门关闭的 run 不拦 fold（hasRunnableProposedActions 与闸门口径一致）。
	if hasRunnableProposedActions(fresh, h.team.Policy) {
		t.Fatal("仅剩闸门关闭动作的 run 不得拦截 fold")
	}
}

// TestPassActionsRequestContextForwards request_context 经 ForwardToMember
// 落地：转发入队（幂等 RequestID 派生）、task_forwarded 增载 actionKey、
// 动作 executed；重复驱动由 journal 事实收敛，零重复副作用。
func TestPassActionsRequestContextForwards(t *testing.T) {
	h := newActionsExecHarness(t)
	h.team.Policy.AutoDispatch = true
	key := ActionKey("team-1", "run-ex", "lead", 1, 0)
	run := execRunFixture(Action{
		Type: ActionTypeRequestContext, SourceTaskID: "lead", Round: 1,
		Member: "rev", Task: "取回设计", FromTaskID: "lead",
		ActionKey: key, Status: ActionStatusProposed,
	})
	state := h.saveExecRun(t, run)
	h.service.passActions(state)
	entries := h.queue.entriesFor("s-rev")
	if len(entries) != 1 {
		t.Fatalf("应恰有一条转发入队，实际 %d", len(entries))
	}
	wantSubmission := ForwardSubmissionID("team-1", "run-ex", ActionRequestID(key))
	if entries[0].submissionID != wantSubmission {
		t.Fatalf("转发 submissionID 应由 ActionKey 派生:\nwant %s\ngot  %s", wantSubmission, entries[0].submissionID)
	}
	for _, want := range []string{"Lead 阶段结论", "取回设计"} {
		if !strings.Contains(entries[0].text, want) {
			t.Fatalf("转发文本应含 %q:\n%s", want, entries[0].text)
		}
	}
	forwarded := h.eventsOf(t, EventTaskForwarded)
	if len(forwarded) != 1 || payloadString(forwarded[0].Payload, "actionKey") != key {
		t.Fatalf("task_forwarded 应增载 actionKey: %+v", forwarded)
	}
	if fresh := h.store.Run("run-ex"); fresh.PendingActions[0].Status != ActionStatusExecuted {
		t.Fatalf("request_context 应落 executed: %+v", fresh.PendingActions)
	}
	// 重复驱动（循环每轮重放）：journal 已有终局事实 → 快照收敛，零重复入队/记账。
	h.service.passActions(state)
	if len(h.queue.entriesFor("s-rev")) != 1 {
		t.Fatalf("收敛路径不得重复入队: %+v", h.queue.entriesFor("s-rev"))
	}
	if len(h.eventsOf(t, EventTaskForwarded)) != 1 {
		t.Fatalf("收敛路径不得重复记账: %d", len(h.eventsOf(t, EventTaskForwarded)))
	}
}

// TestPassActionsDuplicateActionKeyRejected 同 Run 重复动作幂等键：两条同键
// proposed 全部拒绝且原因可解释（§5.2）。
func TestPassActionsDuplicateActionKeyRejected(t *testing.T) {
	h := newActionsExecHarness(t)
	key := ActionKey("team-1", "run-ex", "lead", 1, 0)
	duplicate := Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
		Member: "dev", Task: "实现接口",
		ActionKey: key, Status: ActionStatusProposed,
	}
	run := execRunFixture(duplicate, duplicate)
	state := h.saveExecRun(t, run)
	h.service.passActions(state)
	fresh := h.store.Run("run-ex")
	if len(fresh.Tasks) != 1 {
		t.Fatalf("重复动作不得创建任务: %+v", fresh.Tasks)
	}
	for i := range fresh.PendingActions {
		if fresh.PendingActions[i].Status != ActionStatusRejected ||
			!strings.Contains(fresh.PendingActions[i].Reason, "重复的动作幂等键") {
			t.Fatalf("同键动作应全部拒绝: %+v", fresh.PendingActions[i])
		}
	}
	if len(h.eventsOf(t, EventActionRejected)) != 2 {
		t.Fatalf("两条拒绝各落一条事件，实际 %d", len(h.eventsOf(t, EventActionRejected)))
	}
}

// TestBuildActionTaskWriteLockNormalization 派生任务写锁归一（§6.4 规则 3）：
// delegate 缺省 exclusive、显式声明 none 生效；summarize 是纯读摘要、缺省
// none（与修复任务 WriteLock=none 同口径——纯读任务持独占写锁会阻塞同项目
// 写者，且 cwd 解析不到时被 ReadyTasks 无声跳过）。
func TestBuildActionTaskWriteLockNormalization(t *testing.T) {
	team := testTeam()
	run := actionsRunFixture()
	cases := []struct {
		name   string
		action Action
		want   string
	}{
		{name: "delegate 缺省 exclusive", action: Action{
			Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev", Task: "实现接口",
			ActionKey: ActionKey("team-1", "run-9", "lead", 1, 0),
		}, want: TaskWriteLockExclusive},
		{name: "delegate 声明 none", action: Action{
			Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev", Task: "审查实现",
			WriteLock: TaskWriteLockNone,
			ActionKey: ActionKey("team-1", "run-9", "lead", 1, 1),
		}, want: TaskWriteLockNone},
		{name: "summarize 缺省 none", action: Action{
			Type: ActionTypeSummarize, SourceTaskID: "lead", Round: 2,
			ActionKey: ActionKey("team-1", "run-9", "lead", 2, 0),
		}, want: TaskWriteLockNone},
		{name: "summarize 显式 exclusive", action: Action{
			Type: ActionTypeSummarize, SourceTaskID: "lead", Round: 2,
			WriteLock: TaskWriteLockExclusive,
			ActionKey: ActionKey("team-1", "run-9", "lead", 2, 1),
		}, want: TaskWriteLockExclusive},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			action := testCase.action
			task := buildActionTask(run, team, &action)
			if task.WriteLock != testCase.want {
				t.Fatalf("派生任务写锁应为 %s，实际 %s（%+v）", testCase.want, task.WriteLock, task)
			}
		})
	}
	// ValidateActions 把信封里的 writeLock 落进 Action（缺省空串，归一交给
	// buildActionTask）。
	payload, violations := decodeActionPayload(actionJSON(
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"审查实现","writeLock":"none"}`))
	if len(violations) != 0 {
		t.Fatalf("声明 writeLock=none 的 delegate 应校验通过，实际 %v", violations)
	}
	actions, violations := ValidateActions(payload, team, run, DefaultPolicy())
	if len(violations) != 0 || len(actions) != 1 {
		t.Fatalf("校验产物不符: actions=%v violations=%v", actions, violations)
	}
	if actions[0].WriteLock != TaskWriteLockNone {
		t.Fatalf("动作应携带 writeLock=none: %+v", actions[0])
	}
}

// TestPassActionsTransientSessionOpenFailureNotRejected 「归属明确但当前打不
// 开」的会话（openACP 瞬态/环境性失败：agent 冷启超时/被桌面端持有/暂无 cwd）
// 不是绑定失效：不落一次性终局拒绝（无 action_rejected、无 Reason），动作照
// 常执行——与 executeDispatch 对同类 Resolve 失败仅降级 AIMD 桶键、任务照常
// 入队的语义对齐（派生任务的派发由队列与会话恢复兜底）。只有「不属于任何
// ACP Agent」（manager 与 session 双 nil）才落 action_rejected（§5.2 绑定
// 会话失效）。
func TestPassActionsTransientSessionOpenFailureNotRejected(t *testing.T) {
	h := newActionsExecHarness(t)
	key := ActionKey("team-1", "run-ex", "lead", 1, 0)
	run := execRunFixture(Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
		Member: "dev", Task: "实现接口",
		ActionKey: key, Status: ActionStatusProposed,
	})
	state := h.saveExecRun(t, run)
	// dev 会话归属明确但打不开（生产 openACP 失败的形态）。
	h.resolver.setUnopenable("s-dev", errors.New("此会话正在桌面 App 中打开"))
	h.service.passActions(state)
	fresh := h.store.Run("run-ex")
	if fresh.PendingActions[0].Status != ActionStatusExecuted {
		t.Fatalf("瞬态打开失败不得终局拒绝动作: %+v", fresh.PendingActions[0])
	}
	if fresh.PendingActions[0].Reason != "" {
		t.Fatalf("非拒绝动作不应携带拒绝原因: %+v", fresh.PendingActions[0])
	}
	if len(h.eventsOf(t, EventActionRejected)) != 0 {
		t.Fatal("瞬态打开失败不得落 action_rejected")
	}
	if derived := findTaskIn(fresh, "lead-d1-0"); derived == nil {
		t.Fatal("瞬态打开失败应照常创建派生任务（派发路径与会话恢复兜底）")
	}
}

// TestPassActionsUnknownSessionRejected 会话不属于任何 ACP Agent
// （manager 与 session 双 nil：已删除/从未接入）才是 §5.2 的「绑定会话已
// 失效」——一次性可解释拒绝。
func TestPassActionsUnknownSessionRejected(t *testing.T) {
	h := newActionsExecHarness(t)
	// 摘掉 dev 的会话与归属（manager、session 均 nil）。
	h.resolver.setSession("s-dev", nil)
	key := ActionKey("team-1", "run-ex", "lead", 1, 0)
	run := execRunFixture(Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
		Member: "dev", Task: "实现接口",
		ActionKey: key, Status: ActionStatusProposed,
	})
	state := h.saveExecRun(t, run)
	h.service.passActions(state)
	fresh := h.store.Run("run-ex")
	if fresh.PendingActions[0].Status != ActionStatusRejected ||
		!strings.Contains(fresh.PendingActions[0].Reason, "绑定会话已失效") {
		t.Fatalf("无归属会话应落一次性拒绝: %+v", fresh.PendingActions[0])
	}
	if len(h.eventsOf(t, EventActionRejected)) != 1 {
		t.Fatalf("应恰一条 action_rejected，实际 %d", len(h.eventsOf(t, EventActionRejected)))
	}
}
