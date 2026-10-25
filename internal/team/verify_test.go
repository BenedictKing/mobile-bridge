package team

// verify_test.go 覆盖确定性验证闸门（§6.4.1、§13.1 team/verification 行）：
// 白名单校验表驱动（make check 过 / sh -c 拒 / 元字符拒 / 超长拒）、固定
// argv 无 shell（PATH 注入伪命令记录实际收到的参数，含空格参数不拆）、
// 退出码分支（0→completed、7→waiting_input+run_verify 事件、超时/cwd 解析
// 失败→-1）、resume 复验重跑闸门、输出截断、RunReducing 分支不重复启动闸门
// （gateLaunched 断言）、闸门期 Cancel 丢弃结果（first-wins）、字段序列化
// 往返。伪命令是测试脚手架（shebang 直 exec）；闸门本身仍走
// exec.CommandContext 固定 argv。等待全部 deadline 轮询，不用固定 sleep 判
// 定正确性（唯一例外：防重复启动用例的静默窗口，见该处注释——判据是调用
// 计数，不是时长）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 白名单校验（ValidateVerifyCommand）----

func TestValidateVerifyCommand(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr string // 空=应通过
	}{
		{"make check 通过", []string{"make", "check"}, ""},
		{"裸 make 通过", []string{"make"}, ""},
		{"go test 带包参数通过", []string{"go", "test", "./internal/team/..."}, ""},
		{"go vet 通过", []string{"go", "vet"}, ""},
		{"pytest 通过", []string{"pytest"}, ""},
		{"pytest 带参数通过", []string{"pytest", "-q", "tests/"}, ""},
		{"pnpm lint 通过", []string{"pnpm", "lint"}, ""},
		{"空命令拒绝", nil, "不能为空"},
		{"段数超限拒绝", []string{"make", "a", "b", "c", "d", "e", "f", "g", "h"}, "最多 8 段"},
		{"空段拒绝", []string{"make", ""}, "每一段都不能为空"},
		{"sh -c 拒绝", []string{"sh", "-c", "make check"}, "不在允许清单内"},
		{"bash 拒绝", []string{"bash", "-c", "true"}, "不在允许清单内"},
		{"管道元字符拒绝", []string{"make", "a|b"}, "不支持 shell 语法"},
		{"分号元字符拒绝", []string{"make", "a;b"}, "不支持 shell 语法"},
		{"反引号元字符拒绝", []string{"make", "a`b`"}, "不支持 shell 语法"},
		{"美元元字符拒绝", []string{"make", "$HOME"}, "不支持 shell 语法"},
		{"重定向元字符拒绝", []string{"make", ">out"}, "不支持 shell 语法"},
		{"换行元字符拒绝", []string{"make", "a\nb"}, "不支持 shell 语法"},
		{"大小写敏感拒绝", []string{"Make", "check"}, "不在允许清单内"},
		{"npm run 不在清单", []string{"npm", "run"}, "不在允许清单内"},
		{"前缀不完整拒绝", []string{"go"}, "不在允许清单内"},
		{"单段超长拒绝", []string{"make", strings.Repeat("x", 513)}, "单段不能超过 512 字节"},
	}
	for _, testCase := range cases {
		err := ValidateVerifyCommand(testCase.argv)
		if testCase.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: 应通过，实际 %v", testCase.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
			t.Fatalf("%s: 应报 %q，实际 %v", testCase.name, testCase.wantErr, err)
		}
	}
}

// ---- 闸门执行（真实 exec 路径 + PATH 注入伪命令）----

// shimVerifyBin 在 PATH 前部注入伪命令 name：每次调用向 $VERIFY_ARGV_FILE
// 追加「invoke 标记 + 每参数一行」（参数含空格/特殊字符也不经任何 shell 拆
// 分——脚本由闸门的 exec 直连启动，$#/$@ 是内核给的原始 argv）。随后可选
// 输出指定字节数（VERIFY_STDOUT_BYTES/VERIFY_STDERR_BYTES）、按
// VERIFY_HOLD_SECS 睡眠（超时/取消窗口用）、按 VERIFY_EXIT 退出。
func shimVerifyBin(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	content := `#!/bin/sh
{ echo invoke; for arg in "$@"; do printf '%s\n' "$arg"; done; } >> "$VERIFY_ARGV_FILE"
if [ -n "$VERIFY_STDOUT_BYTES" ]; then head -c "$VERIFY_STDOUT_BYTES" /dev/zero | tr '\0' o; fi
if [ -n "$VERIFY_STDERR_BYTES" ]; then head -c "$VERIFY_STDERR_BYTES" /dev/zero | tr '\0' e >&2; fi
if [ -n "$VERIFY_HOLD_SECS" ]; then sleep "$VERIFY_HOLD_SECS"; fi
if [ -n "$VERIFY_EXIT" ]; then exit "$VERIFY_EXIT"; fi
exit 0
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// readVerifyInvocations 解析伪命令的 argv 记录：invoke 标记分组，组内每行
// 一个参数（空文件=尚未被调用）。
func readVerifyInvocations(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var invocations [][]string
	var current []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "invoke" {
			if current != nil {
				invocations = append(invocations, current)
			}
			current = []string{}
			continue
		}
		current = append(current, line)
	}
	if current != nil {
		invocations = append(invocations, current)
	}
	return invocations
}

// newVerifyHarness 装配闸门基座：注入伪 make、Lead 会话 cwd 指向真实目录
// （闸门命令的 Dir）、argv 记录文件就位、闸门超时缩到秒级以下可覆盖。
func newVerifyHarness(t *testing.T) (*dispatchHarness, string) {
	t.Helper()
	h := newDispatchHarness(t)
	workDir := t.TempDir()
	h.resolver.setCwd("s-lead", workDir)
	shimVerifyBin(t, "make")
	t.Setenv("VERIFY_ARGV_FILE", filepath.Join(t.TempDir(), "argv.log"))
	h.service.verifyTimeout = 5 * time.Second
	return h, workDir
}

// createVerifyRun 建一个 single 模式（合成 Lead 任务）+验证闸门的 run 并把
// 任务推到 completed（闸门只在全任务终态后启动）。
func completeVerifyRunTask(t *testing.T, h *dispatchHarness, run *Run) {
	t.Helper()
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务入队")
	h.completeSubmission("s-lead", submissionID, sentText, "实现完成。")
	waitFor(t, "任务 completed", func() bool {
		return h.taskOf(run.ID, "lead").Status == TaskCompleted
	})
}

// verifyEventsOf 重放 journal 并返回 run 的 run_verify 事件 payload 列表。
func verifyEventsOf(t *testing.T, journal *Journal, runID string) []map[string]any {
	t.Helper()
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	var payloads []map[string]any
	for _, event := range events {
		if event.RunID == runID && event.Type == EventRunVerify {
			payloads = append(payloads, event.Payload)
		}
	}
	return payloads
}

func gateLaunchedOf(h *dispatchHarness, runID string) bool {
	state := h.service.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.gateLaunched
}

// TestVerifyGateFixedArgvNoShell 固定 argv 直连 exec：伪 make 收到的参数与
// 声明逐字相等（含空格的参数不被拆分），无 shell 参与。
func TestVerifyGateFixedArgvNoShell(t *testing.T) {
	h, _ := newVerifyHarness(t)
	argvFile := os.Getenv("VERIFY_ARGV_FILE")
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "",
		[]string{"make", "check", "pkg with space"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "闸门执行并完成 run", func() bool {
		return h.store.Run(run.ID).Status == RunCompleted
	})
	invocations := readVerifyInvocations(t, argvFile)
	if len(invocations) != 1 {
		t.Fatalf("闸门应恰好执行一次，实际 %d 次: %v", len(invocations), invocations)
	}
	want := []string{"check", "pkg with space"}
	if !equalStrings(invocations[0], want) {
		t.Fatalf("伪命令收到的参数应逐字相等（空格参数不拆）: 想要 %q 实际 %q", want, invocations[0])
	}
}

// TestVerifyGateExitZeroCompletesRun 退出码 0 → 按 FoldRunStatus 正常 fold
// （completed），VerifyResult 随快照持久化，run_verify 事件带 exitCode 0。
func TestVerifyGateExitZeroCompletesRun(t *testing.T) {
	h, _ := newVerifyHarness(t)
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "闸门通过 run 完成", func() bool {
		return h.store.Run(run.ID).Status == RunCompleted
	})
	fresh := h.store.Run(run.ID)
	if fresh.Verify == nil || fresh.Verify.ExitCode != 0 {
		t.Fatalf("闸门结果应持久化 exitCode=0: %+v", fresh.Verify)
	}
	if fresh.Verify.FinishedAt.IsZero() {
		t.Fatalf("闸门结果缺 finishedAt: %+v", fresh.Verify)
	}
	payloads := verifyEventsOf(t, h.journal, run.ID)
	if len(payloads) != 1 {
		t.Fatalf("应恰好一条 run_verify 事件，实际 %d", len(payloads))
	}
	if code, _ := payloads[0]["exitCode"].(json.Number); code.String() != "0" {
		t.Fatalf("run_verify 事件应带 exitCode=0: %v", payloads[0])
	}
	if _, exists := payloads[0]["durationMs"]; !exists {
		t.Fatalf("run_verify 事件应带 durationMs: %v", payloads[0])
	}
}

// TestVerifyGateNonZeroPausesWaitingInput 非零退出码（7）→ run waiting_input，
// stopReason=verify_failed（退出码 7），事件落账；Agent 的「完成」声明不足以
// 终局（§13.3.7）。
func TestVerifyGateNonZeroPausesWaitingInput(t *testing.T) {
	h, _ := newVerifyHarness(t)
	t.Setenv("VERIFY_EXIT", "7")
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "闸门未过 run 暂停", func() bool {
		return h.store.Run(run.ID).Status == RunWaitingInput
	})
	fresh := h.store.Run(run.ID)
	if fresh.StopReason != "verify_failed（退出码 7）" {
		t.Fatalf("停靠原因应为 verify_failed（退出码 7），实际 %q", fresh.StopReason)
	}
	if fresh.Verify == nil || fresh.Verify.ExitCode != 7 {
		t.Fatalf("闸门结果应持久化 exitCode=7: %+v", fresh.Verify)
	}
	payloads := verifyEventsOf(t, h.journal, run.ID)
	if len(payloads) != 1 {
		t.Fatalf("应恰好一条 run_verify 事件，实际 %d", len(payloads))
	}
	if code, _ := payloads[0]["exitCode"].(json.Number); code.String() != "7" {
		t.Fatalf("run_verify 事件应带 exitCode=7: %v", payloads[0])
	}
}

// TestVerifyGateResumeRerunsAndPasses 复验闭环：exit 7 停靠 → 修复（伪命令
// 改为退出 0）→ resume 清 Verify 重跑闸门 → exit 0 → run completed，两次
// run_verify 事实都在 journal。
func TestVerifyGateResumeRerunsAndPasses(t *testing.T) {
	h, _ := newVerifyHarness(t)
	t.Setenv("VERIFY_EXIT", "7")
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "首次闸门未过", func() bool {
		return h.store.Run(run.ID).Status == RunWaitingInput
	})
	// 「修复」：伪命令退出码改回 0，resume 复验。
	t.Setenv("VERIFY_EXIT", "")
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatalf("resume 复验失败: %v", err)
	}
	waitFor(t, "复验通过 run 完成", func() bool {
		return h.store.Run(run.ID).Status == RunCompleted
	})
	fresh := h.store.Run(run.ID)
	if fresh.Verify == nil || fresh.Verify.ExitCode != 0 {
		t.Fatalf("复验结果应为 exitCode=0: %+v", fresh.Verify)
	}
	if fresh.StopReason != "" {
		t.Fatalf("复验通过后停靠原因应清空，实际 %q", fresh.StopReason)
	}
	payloads := verifyEventsOf(t, h.journal, run.ID)
	if len(payloads) != 2 {
		t.Fatalf("两次闸门应各落一条 run_verify 事件，实际 %d", len(payloads))
	}
	if code, _ := payloads[1]["exitCode"].(json.Number); code.String() != "0" {
		t.Fatalf("第二次 run_verify 事件应带 exitCode=0: %v", payloads[1])
	}
}

// TestVerifyGateTimeoutFailsClosed 命令超时（context 600s 的测试替身压到
// 200ms）→ exit=-1 + reason=timeout，run 停靠等待修复后复验。
func TestVerifyGateTimeoutFailsClosed(t *testing.T) {
	h, _ := newVerifyHarness(t)
	h.service.verifyTimeout = 200 * time.Millisecond
	t.Setenv("VERIFY_HOLD_SECS", "3")
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "闸门超时 run 暂停", func() bool {
		return h.store.Run(run.ID).Status == RunWaitingInput
	})
	fresh := h.store.Run(run.ID)
	if fresh.Verify == nil || fresh.Verify.ExitCode != -1 {
		t.Fatalf("超时应归 exitCode=-1: %+v", fresh.Verify)
	}
	if fresh.StopReason != "verify_failed（退出码 -1）" {
		t.Fatalf("停靠原因应为 verify_failed（退出码 -1），实际 %q", fresh.StopReason)
	}
	payloads := verifyEventsOf(t, h.journal, run.ID)
	if len(payloads) != 1 {
		t.Fatalf("应恰好一条 run_verify 事件，实际 %d", len(payloads))
	}
	if reason, _ := payloads[0]["reason"].(string); reason != "timeout" {
		t.Fatalf("超时事件的 reason 应为 timeout: %v", payloads[0])
	}
	// 清理：让闸门进程尽快退出（3s 睡眠被 ctx 杀掉，无需等待）。
	_, _ = h.service.Cancel(run.ID)
}

// TestVerifyGateCwdUnresolved Lead 会话 cwd 解析不到 → 闸门不执行命令，
// exit=-1 + reason=cwd_unresolved，run 停靠。任务用显式 writeLock:none 的
// single 计划（合成 Lead 任务默认 exclusive，缺 cwd 时调度本就不派发）。
func TestVerifyGateCwdUnresolved(t *testing.T) {
	h, _ := newVerifyHarness(t)
	h.resolver.setCwd("s-lead", "") // 模拟会话已不可解析
	plan := []PlanTaskInput{{ID: "lead", MemberID: "lead", Prompt: "产出实现", WriteLock: TaskWriteLockNone}}
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, plan)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "cwd 解析失败 run 暂停", func() bool {
		return h.store.Run(run.ID).Status == RunWaitingInput
	})
	fresh := h.store.Run(run.ID)
	if fresh.Verify == nil || fresh.Verify.ExitCode != -1 {
		t.Fatalf("cwd 未解析应归 exitCode=-1: %+v", fresh.Verify)
	}
	if !strings.Contains(fresh.Verify.OutputHead, "无法解析") {
		t.Fatalf("结果应说明未执行原因: %+v", fresh.Verify)
	}
	payloads := verifyEventsOf(t, h.journal, run.ID)
	if len(payloads) != 1 {
		t.Fatalf("应恰好一条 run_verify 事件，实际 %d", len(payloads))
	}
	if reason, _ := payloads[0]["reason"].(string); reason != "cwd_unresolved" {
		t.Fatalf("事件的 reason 应为 cwd_unresolved: %v", payloads[0])
	}
	_, _ = h.service.Cancel(run.ID)
}

// TestVerifyGateOutputHeadTruncated stdout 5000 字节 + stderr 9000 字节：
// 捕获层各限 8192（stderr 截断）、outputHead 限 2048 字节（UTF-8 边界），
// Truncated 置位。
func TestVerifyGateOutputHeadTruncated(t *testing.T) {
	h, _ := newVerifyHarness(t)
	t.Setenv("VERIFY_STDOUT_BYTES", "5000")
	t.Setenv("VERIFY_STDERR_BYTES", "9000")
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "闸门完成 run 完成", func() bool {
		return h.store.Run(run.ID).Status == RunCompleted
	})
	fresh := h.store.Run(run.ID)
	if fresh.Verify == nil {
		t.Fatal("闸门结果缺失")
	}
	if size := len(fresh.Verify.OutputHead); size > 2048 {
		t.Fatalf("outputHead 应 ≤2048 字节，实际 %d", size)
	}
	if !fresh.Verify.Truncated {
		t.Fatalf("双层截断都未触发时才允许 Truncated=false: %q", fresh.Verify.OutputHead)
	}
}

// TestVerifyGateRunReducingNoDoubleLaunch 闸门进行中（伪命令睡 2s）的
// RunReducing 窗口：gateLaunched 置位、run 保持闸门态；2s 窗口内等待片
// 2ms 的调度循环会经过状态分支上千轮——终局后的调用计数恒为 1 即证明分支
// 幂等（不重复启动闸门），run 也未在闸门完成前被误 fold。
func TestVerifyGateRunReducingNoDoubleLaunch(t *testing.T) {
	h, _ := newVerifyHarness(t)
	argvFile := os.Getenv("VERIFY_ARGV_FILE")
	t.Setenv("VERIFY_HOLD_SECS", "2")
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "run 进入闸门态", func() bool {
		return h.store.Run(run.ID).Status == RunReducing
	})
	if !gateLaunchedOf(h, run.ID) {
		t.Fatal("RunReducing 态 gateLaunched 应置位")
	}
	// 闸门完成（伪命令睡满 2s 退出）后的终局是确定同步点：窗口内任何一轮
	// 循环的重复启动都会落在 argv 记录里，等终局再断言即可，无需计时。
	// 15s 上限：伪命令是真实 2s 子进程睡眠，make check 全仓并行（含 -race
	// 轮）时包级负载可让 sleep+收尾超出 waitFor 的 5s 默认上限（曾实测超时）。
	if !waitForSoft(15*time.Second, "闸门完成 run 完成", func() bool {
		return h.store.Run(run.ID).Status == RunCompleted
	}) {
		t.Fatalf("等待超时: 闸门完成 run 完成")
	}
	invocations := readVerifyInvocations(t, argvFile)
	if len(invocations) != 1 {
		t.Fatalf("闸门全程应恰好一次调用（分支不得重复启动），实际 %d 次: %v", len(invocations), invocations)
	}
	if !equalStrings(invocations[0], []string{"check"}) {
		t.Fatalf("闸门参数应逐字传递: %q", invocations[0])
	}
}

// TestVerifyGateCancelDuringGateDiscardsResult 闸门期 Cancel：既有路径把 run
// 落 cancelled（first-wins），闸门 goroutine 完成后发现 run 终态丢弃结果——
// 不落 run_verify 事件、不复活 run。
func TestVerifyGateCancelDuringGateDiscardsResult(t *testing.T) {
	h, _ := newVerifyHarness(t)
	t.Setenv("VERIFY_HOLD_SECS", "2")
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	completeVerifyRunTask(t, h, run)
	waitFor(t, "run 进入闸门态", func() bool {
		return h.store.Run(run.ID).Status == RunReducing
	})
	if _, err := h.service.Cancel(run.ID); err != nil {
		t.Fatalf("闸门期取消失败: %v", err)
	}
	if status := h.store.Run(run.ID).Status; status != RunCancelled {
		t.Fatalf("取消应立即落 cancelled，实际 %s", status)
	}
	// 等闸门 goroutine 收尾（gateLaunched 复位是可观测信号）。15s 上限：
	// 闸门伪命令的真实 2s 子进程睡眠在 make check 全仓并行（含 -race 轮）
	// 负载下可能超出 waitFor 的 5s 默认上限（曾实测超时）。
	if !waitForSoft(15*time.Second, "闸门 goroutine 收尾", func() bool { return !gateLaunchedOf(h, run.ID) }) {
		t.Fatalf("等待超时: 闸门 goroutine 收尾")
	}
	fresh := h.store.Run(run.ID)
	if fresh.Status != RunCancelled {
		t.Fatalf("闸门结果不得复活已取消的 run，实际 %s", fresh.Status)
	}
	if fresh.Verify != nil {
		t.Fatalf("取消后的闸门结果应丢弃，实际 %+v", fresh.Verify)
	}
	if payloads := verifyEventsOf(t, h.journal, run.ID); len(payloads) != 0 {
		t.Fatalf("取消后的闸门不应落 run_verify 事件，实际 %d 条", len(payloads))
	}
}

// TestVerifyGateNotRunWhenTaskFailed 闸门只拦「fold 判 completed」的终局：
// 任务失败直接 fold failed，不跑闸门（失败 run 无成功声明可验证）。
func TestVerifyGateNotRunWhenTaskFailed(t *testing.T) {
	h, _ := newVerifyHarness(t)
	run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	submissionID, _ := h.waitForSubmission(t, "s-lead", "lead 任务入队")
	h.queue.setStatus("s-lead", submissionID, SubStatusFailed, "agent 拒绝执行")
	waitFor(t, "run 失败", func() bool {
		return h.store.Run(run.ID).Status == RunFailed
	})
	if payloads := verifyEventsOf(t, h.journal, run.ID); len(payloads) != 0 {
		t.Fatalf("失败的 run 不应执行闸门，实际 %d 条 run_verify", len(payloads))
	}
	if fresh := h.store.Run(run.ID); fresh.Verify != nil {
		t.Fatalf("失败的 run 不应落闸门结果: %+v", fresh.Verify)
	}
}

// ---- 序列化往返（§8.1 未知字段保留 + 新字段落盘）----

// TestVerifyFieldsSurviveStoreRoundtrip verifyCommand/verify/sentText 经
// JSON Store 写盘重载后仍在（漏 Set 或漏 knownFields 都会让字段静默丢失）。
func TestVerifyFieldsSurviveStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	teamRow := testTeam()
	if err := store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	finished := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	run := &Run{
		ID: "run-verify-rt", TeamID: teamRow.ID, Goal: "目标", Mode: ModeSingle,
		CollectPolicy: RunCollectAll, Status: RunWaitingInput, Round: 1, PlanHash: "hash",
		VerifyCommand: []string{"make", "check"},
		Verify:        &VerifyResult{ExitCode: 7, OutputHead: "FAIL", Truncated: true, FinishedAt: finished},
		Tasks: []Task{{
			ID: "lead", MemberID: "lead", Prompt: "目标", Status: TaskCompleted,
			SentText: "[Team context]\n来源成员：dev\n\n结果\n\n[Assigned task]\n目标",
		}},
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	reloaded := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	fresh := reloaded.Run(run.ID)
	if fresh == nil {
		t.Fatal("run 重载失败")
	}
	if !equalStrings(fresh.VerifyCommand, []string{"make", "check"}) {
		t.Fatalf("verifyCommand 往返丢失: %v", fresh.VerifyCommand)
	}
	if fresh.Verify == nil || fresh.Verify.ExitCode != 7 || fresh.Verify.OutputHead != "FAIL" ||
		!fresh.Verify.Truncated || !fresh.Verify.FinishedAt.Equal(finished) {
		t.Fatalf("verify 往返丢失或失真: %+v", fresh.Verify)
	}
	if got := fresh.Tasks[0].SentText; got != run.Tasks[0].SentText {
		t.Fatalf("sentText 往返丢失: %q", got)
	}
	// 事件侧的 run_created 载荷带 verifyCommand（journal 重建的来源）。
	journal, err := OpenJournal(filepath.Join(dir, "team-journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(reloaded, journal, newFakeQueue(), newFakeResolver())
	created, err := service.CreateRun(teamRow, "", "目标二", ModeSingle, "", []string{"pytest", "-q"}, nil)
	if err != nil {
		t.Fatalf("创建带闸门的 run 失败: %v", err)
	}
	events, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	var createdPayload map[string]any
	for _, event := range events {
		if event.RunID == created.ID && event.Type == EventRunCreated {
			createdPayload = event.Payload
		}
	}
	recorded, _ := createdPayload["verifyCommand"].([]any)
	if len(recorded) != 2 || recorded[0] != "pytest" || recorded[1] != "-q" {
		t.Fatalf("run_created 载荷应带 verifyCommand: %v", createdPayload)
	}
	// 收尾：取消 run 让调度循环先于 t.TempDir 清理退出（写盘撞目录删除会让
	// 测试收尾报 directory not empty）。
	_, _ = service.Cancel(created.ID)
}
