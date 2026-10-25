package team

// verify.go 是确定性验证闸门（§6.4.1、§12 工作项 7、§13.1 team/verification
// 行）：Run 声明固定 argv 的验证命令（创建期白名单校验，ValidateVerifyCommand），
// 全任务终态且 fold 为 completed 时先过闸门再终局——Dispatcher 用无 shell 的
// exec.CommandContext 执行，非零退出码作为验证结果交给判定（§13.3.7：暂停或
// 失败取暂停，可修可重跑），不把 Agent 的口头声明当作测试证据。
//
// 状态推进与执行序（runner.go loopPass）：
//
//	passFold ④：FoldRunStatus 判 completed 且闸门未决（verifyPending）→
//	  run 推进 RunReducing + 启动闸门（ensureVerifyGateLocked）；
//	passAdvance 状态段 RunReducing 分支：短路 ①②③（不收养、不级联、不派发，
//	  防闸门进行中重复派发），未启动则启动，已启动未出结果则等片；
//	闸门 goroutine：锁外 exec（≤verifyTimeout）→ commitRun 落 VerifyResult +
//	  run_verify 事件 → applyVerifyVerdictLocked 判定 → wake 循环。
//
// 崩溃窗口自愈：run 停在 RunReducing 且 Verify==nil（重启/记账失败）→ 分支
// 重新启动闸门（重跑命令幂等）；Verify 已落但判定未应用 → 分支就地补判。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// verifyCommandAllowlist 是闸门命令的白名单（§16.8 默认决策 8：只认仓库已
// 登记的 make / 测试 / lint 入口，禁止任意 shell 字符串）。前缀匹配：argv 以
// 某条目为前缀即放行——{"make"} 覆盖 make check，{"go","test"} 覆盖带包参数
// 的 go test。编译期固定，不做配置化/热更新（显式非目标）。
var verifyCommandAllowlist = [][]string{
	{"make"},
	{"go", "test"},
	{"go", "vet"},
	{"npm", "test"},
	{"pnpm", "test"},
	{"pnpm", "lint"},
	{"yarn", "test"},
	{"yarn", "lint"},
	{"pytest"},
}

// 闸门命令的形状护栏（ValidateVerifyCommand）。
const (
	// verifyCommandMaxArgs 是 argv 的最大段数（含命令名）。
	verifyCommandMaxArgs = 8
	// verifyArgMaxBytes 是单段的最大字节数：命令本体已在白名单内，附加参数
	// （包路径、测试名等）远用不到这个量级；封顶保证 verifyCommand 进入
	// run_created 事件与 Run 快照时恒有界（§10.3）。
	verifyArgMaxBytes = 512
	// verifyShellMetachars 是整串禁止出现的 shell 元字符（|;&$`>< 与换行）。
	verifyShellMetachars = "|;&$`><\n"
)

// ValidateVerifyCommand 校验 Run 声明的验证命令（§6.4.1/§16.8）：固定 argv、
// 无 shell。规则：1–8 段、每段非空且 ≤512 字节、以白名单某条目为前缀、整串
// 不含 shell 元字符。空 argv 报错（调用方对「未声明闸门」自行跳过校验）。
// 中文报错供 HTTP 侧直接映射 400。
func ValidateVerifyCommand(argv []string) error {
	if len(argv) == 0 {
		return errors.New("验证命令不能为空")
	}
	if len(argv) > verifyCommandMaxArgs {
		return fmt.Errorf("验证命令最多 %d 段（含命令名），收到 %d 段", verifyCommandMaxArgs, len(argv))
	}
	for _, part := range argv {
		if part == "" {
			return errors.New("验证命令的每一段都不能为空")
		}
		if len(part) > verifyArgMaxBytes {
			return fmt.Errorf("验证命令的单段不能超过 %d 字节：%s", verifyArgMaxBytes, part)
		}
		if strings.ContainsAny(part, verifyShellMetachars) {
			return fmt.Errorf("验证命令不支持 shell 语法（发现元字符），请拆成固定参数：%s", part)
		}
	}
	for _, prefix := range verifyCommandAllowlist {
		if len(prefix) <= len(argv) && equalArgvPrefix(prefix, argv) {
			return nil
		}
	}
	return fmt.Errorf("验证命令不在允许清单内：%s（可用入口：%s）",
		strings.Join(argv, " "), verifyAllowlistSummary())
}

// equalArgvPrefix 报告 argv 是否以 prefix 逐段相等开头。
func equalArgvPrefix(prefix, argv []string) bool {
	for i, part := range prefix {
		if argv[i] != part {
			return false
		}
	}
	return true
}

// verifyAllowlistSummary 渲染白名单的人类可读摘要（报错用）。
func verifyAllowlistSummary() string {
	entries := make([]string, 0, len(verifyCommandAllowlist))
	for _, prefix := range verifyCommandAllowlist {
		entries = append(entries, strings.Join(prefix, " "))
	}
	return strings.Join(entries, "、")
}

// verifyPending 报告 run 是否声明了验证闸门且闸门结果未落（fold 拦截的判定
// 条件；Verify!=nil 表示闸门已执行——resume 复验会清空它重新置为未决）。
func verifyPending(run *Run) bool {
	return run != nil && len(run.VerifyCommand) > 0 && run.Verify == nil
}

// StopReasonVerifyFailed 是闸门未通过时 run 停靠原因的固定前缀（resume 据
// 此识别「清 Verify 重跑闸门」的复验语义，§13.3.7）。
const StopReasonVerifyFailed = "verify_failed"

// 闸门执行的环境归因（run_verify 事件的 reason 字段；ExitCode=-1 的两种
// 「命令未给出退出码」情形与正常非零退出码区分开）。
const (
	verifyReasonCwdUnresolved = "cwd_unresolved"
	verifyReasonTimeout       = "timeout"
)

// 闸门执行的界（§6.4.1/§10.3）。
const (
	// defaultVerifyTimeout 是闸门命令的执行超时。
	defaultVerifyTimeout = 600 * time.Second
	// verifyOutputCapture 是 stdout/stderr 各自的字节捕获上限（超限截断）。
	verifyOutputCapture = 8192
	// verifyHeadBytes 是 outputHead 的字节上限（事件 payload 与快照共用）。
	verifyHeadBytes = 2048
)

// verifyOutcome 是一次闸门执行的原材料：result 落 Run 快照（VerifyResult），
// reason/durationMs 只进 run_verify 事件 payload（执行环境归因，不进快照）。
type verifyOutcome struct {
	result     VerifyResult
	reason     string
	durationMs int
}

// cappedBuffer 是带字节上限的捕获缓冲（io.Writer）：超限静默丢弃并标记
// truncated（命令的输出流不会被反向限流，只是不无限记账）。
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() >= b.limit {
		b.truncated = true
		return len(p), nil
	}
	room := b.limit - b.buf.Len()
	if len(p) > room {
		b.truncated = true
		b.buf.Write(p[:room])
		return len(p), nil
	}
	return b.buf.Write(p)
}

// executeVerifyCommand 用固定 argv 执行闸门命令（§6.4.1：exec.CommandContext
// 直连、绝不经 shell）。Dir=Lead 成员会话的 cwd（resolver.Cwd）；解析不到 →
// exit=-1 + reason=cwd_unresolved。stdout/stderr 合并捕获（各限 8192 字节，
// 超限标 truncated），outputHead 截 2048 字节（按 UTF-8 边界回退）。超时归
// exit=-1 + reason=timeout（已捕获的部分输出仍随结果返回）。不持任何锁，
// 由闸门 goroutine 在锁外调用。
func (s *Service) executeVerifyCommand(teamID string, argv []string) verifyOutcome {
	started := time.Now()
	timeout := s.verifyTimeout
	if timeout <= 0 {
		timeout = defaultVerifyTimeout
	}
	dir := ""
	if team := s.store.Team(teamID); team != nil && team.LeadID != "" {
		if member := memberOf(team, team.LeadID); member != nil {
			dir = s.resolver.Cwd(member.SessionID)
		}
	}
	if dir == "" {
		return verifyOutcome{
			result: VerifyResult{
				ExitCode:   -1,
				OutputHead: "无法解析 Lead 会话的工作目录，验证命令未执行",
				FinishedAt: time.Now(),
			},
			reason:     verifyReasonCwdUnresolved,
			durationMs: int(time.Since(started).Milliseconds()),
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	stdout := &cappedBuffer{limit: verifyOutputCapture}
	stderr := &cappedBuffer{limit: verifyOutputCapture}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	reason := ""
	if ctx.Err() == context.DeadlineExceeded {
		reason = verifyReasonTimeout
		exitCode = -1
	} else if runErr != nil && cmd.ProcessState == nil {
		// 命令根本没起来（可执行文件不存在等）：exec 已把原因写进 stderr
		// 包装，退出码统一 -1。
		exitCode = -1
	}
	merged := stdout.buf.String()
	if stderr.buf.Len() > 0 {
		if len(merged) > 0 {
			merged += "\n"
		}
		merged += stderr.buf.String()
	}
	head, headTruncated := truncateUTF8(merged, verifyHeadBytes)
	return verifyOutcome{
		result: VerifyResult{
			ExitCode:   exitCode,
			OutputHead: head,
			Truncated:  headTruncated || stdout.truncated || stderr.truncated,
			FinishedAt: time.Now(),
		},
		reason:     reason,
		durationMs: int(time.Since(started).Milliseconds()),
	}
}

// truncateUTF8 按 UTF-8 边界把 text 截到 limit 字节以内（多字节字符不从中间
// 切开，避免账本与事件流出现非法 UTF-8），返回 (截断结果, 是否截断)。
func truncateUTF8(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

// ensureVerifyGateLocked 把「全任务终态、fold 为 completed、闸门未决」的 run
// 推进到 RunReducing 并启动闸门（§6.4.1）。幂等：已在 RunReducing 只补启动
// （重启/超时路径），闸门在飞（gateLaunched）或结果已落（Verify）时不重复。
// 调用方持有 state.mu。passFold ④ 与 expireRun 的 fold 段共用。
func (s *Service) ensureVerifyGateLocked(state *runState, run *Run) {
	if !verifyPending(run) {
		return
	}
	switch run.Status {
	case RunDispatching:
		if err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
			if fresh.Status != RunDispatching {
				return nil, false
			}
			fresh.Status = RunReducing
			return nil, true // 纯状态推进，不落事件（闸门结果才记账）。
		}); err != nil {
			log.Printf("team run %s 验证闸门状态推进失败，下轮重试: %v", run.ID, err)
			return
		}
		fresh := s.store.Run(run.ID)
		if fresh == nil {
			return
		}
		run = fresh
	case RunReducing:
		// 闸门态（重启恢复/超时分支转进来的）：只补启动。
	default:
		return // created（下一轮推进）/waiting_input/终态：不该有未决闸门。
	}
	if run.Status != RunReducing || run.Verify != nil || state.gateLaunched {
		return
	}
	s.launchVerifyGateLocked(state, run)
}

// launchVerifyGateLocked 启动闸门 goroutine 并置 gateLaunched（mu 保护；
// settleVerifyGateLocked 收尾时复位，记账失败时也复位让下一轮重跑）。
// exec 在锁外执行（最长 verifyTimeout），绝不占用 state.mu。
func (s *Service) launchVerifyGateLocked(state *runState, run *Run) {
	state.gateLaunched = true
	runID, teamID := run.ID, run.TeamID
	argv := append([]string(nil), run.VerifyCommand...)
	go func() {
		outcome := s.executeVerifyCommand(teamID, argv)
		state.mu.Lock()
		defer state.mu.Unlock()
		s.settleVerifyGateLocked(state, runID, outcome)
	}()
}

// settleVerifyGateLocked 收账闸门结果（闸门 goroutine 完成；调用方持有
// state.mu）：run 已终态（闸门期被 Cancel 等）丢弃结果（first-wins 既有
// 语义）；否则 commitRun 落 VerifyResult + run_verify 事件，再按结果判定。
// 记账失败（磁盘故障等）复位 gateLaunched，让循环的 RunReducing 分支重跑
// 闸门（验证命令可安全重复执行）。
func (s *Service) settleVerifyGateLocked(state *runState, runID string, outcome verifyOutcome) {
	state.gateLaunched = false
	run := s.store.Run(runID)
	if run == nil || terminalRun(run.Status) {
		return // run 已被并发取消/删除：闸门结果丢弃。
	}
	payload := map[string]any{
		"exitCode":   jsoncompat.Num(outcome.result.ExitCode),
		"durationMs": jsoncompat.Num(outcome.durationMs),
		"outputHead": outcome.result.OutputHead,
		"truncated":  outcome.result.Truncated,
	}
	if outcome.reason != "" {
		payload["reason"] = outcome.reason
	}
	if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) || fresh.Verify != nil {
			return nil, false
		}
		result := outcome.result
		fresh.Verify = &result
		return []*Event{newEvent(runID, EventRunVerify, "runner", payload)}, true
	}); err != nil {
		log.Printf("team run %s 验证闸门结果记账失败（下轮重跑闸门）: %v", runID, err)
		return
	}
	s.applyVerifyVerdictLocked(state, runID)
	s.wakeLocked(state)
}

// applyVerifyVerdictLocked 按闸门结果判定 run 终局（§13.3.7「暂停或失败」取
// 暂停——可修可重跑）：exit!=0（含 cwd 解析失败/超时/启动失败的 -1）→ run
// waiting_input，stopReason=verify_failed（退出码 N），resume 清 Verify 重跑
// 闸门；exit==0 → 按 FoldRunStatus 正常 fold（通常 completed；跨工作流耦合
// 注意：FoldRunStatus 改签名时本调用与 loopPass ④/expireRun/PlanResume 的
// 判定点须一并更新）。幂等：非 RunReducing（已判定/被并发处理）时 no-op。
// 调用方持有 state.mu。
func (s *Service) applyVerifyVerdictLocked(state *runState, runID string) {
	run := s.store.Run(runID)
	if run == nil || run.Verify == nil || run.Status != RunReducing {
		return
	}
	if run.Verify.ExitCode != 0 {
		stopReason := fmt.Sprintf("%s（退出码 %d）", StopReasonVerifyFailed, run.Verify.ExitCode)
		if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
			if fresh.Status != RunReducing {
				return nil, false
			}
			fresh.Status = RunWaitingInput
			fresh.StopReason = stopReason
			return []*Event{newEvent(runID, EventRunWaitingInput, "runner", map[string]any{
				"reason": stopReason,
			})}, true
		}); err != nil {
			log.Printf("team run %s 验证闸门 waiting_input 记账失败（下轮重试）: %v", runID, err)
		}
		return
	}
	if status, stopReason := FoldRunStatus(run.Tasks, run.Mode, run.CollectPolicy); status != "" {
		s.finishRunLocked(state, run, status, stopReason)
		return
	}
	log.Printf("team run %s 验证闸门通过但任务表不再全终态，保持闸门态等待下一轮", runID)
}
