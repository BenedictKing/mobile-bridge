package httpd

// The submissions ledger and queue state machine (Python service.Bridge's
// submissions + maintain loop): messages sent with mode "queue" persist here
// and dispatch to the agent as soon as its turn settles. Outcome branches
// mirror internal/service/submissions.go (the conformance port): a dispatch
// that provably never reached the agent requeues (bounded attempts), a
// definitive rejection fails with the error text, and anything uncertain
// stays "unknown" so the phone asks the user to check the chat instead of
// replaying blindly.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpadapter"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// Submission statuses (Python submissions ledger vocabulary). accepted means
// dispatched to the agent; queued/unknown/failed are visible in the phone's
// queue cards; cancelled/ignored are terminal and hidden.
const (
	SubQueued    = "queued"
	SubUnknown   = "unknown"
	SubAccepted  = "accepted"
	SubFailed    = "failed"
	SubCancelled = "cancelled"
	SubIgnored   = "ignored"
)

// maxQueueAttempts bounds automatic requeues of uncertain outcomes
// (Python _requeue_submission's ladder of 10).
const maxQueueAttempts = 10

type submissionEntry struct {
	ID              string   `json:"id"`
	ThreadID        string   `json:"threadId"`
	Host            string   `json:"host"`
	Text            string   `json:"text"`
	Mode            string   `json:"mode,omitempty"`
	Skills          []string `json:"skills,omitempty"`
	WorkMode        string   `json:"workMode,omitempty"`
	Attachments     []string `json:"attachments,omitempty"`
	AttachmentNames []string `json:"attachmentNames,omitempty"`
	Status          string   `json:"status"`
	Error           string   `json:"error,omitempty"`
	LastQueueError  string   `json:"lastQueueError,omitempty"`
	CreatedAt       float64  `json:"createdAt"`
	QueueAttempts   int      `json:"queueAttempts,omitempty"`
}

// submissions is the durable ledger: key "threadID:submissionID" -> entry,
// insertion-ordered like Python's dict.
type submissions struct {
	mu      sync.Mutex
	path    string
	order   []string
	entries map[string]*submissionEntry
}

// LoadSubmissions reads <dataDir>/submissions.json; a missing or unreadable
// file starts an empty ledger (accepted/uncertain entries survive restarts).
func LoadSubmissions(path string) *submissions {
	ledger := &submissions{path: path, entries: map[string]*submissionEntry{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ledger
	}
	var decoded map[string]*submissionEntry
	if json.Unmarshal(raw, &decoded) == nil {
		for key, entry := range decoded {
			if entry == nil || entry.ID == "" || entry.ThreadID == "" {
				continue
			}
			ledger.entries[key] = entry
			ledger.order = append(ledger.order, key)
		}
	}
	return ledger
}

func submissionKey(threadID, submissionID string) string {
	return threadID + ":" + submissionID
}

func (q *submissions) get(threadID, submissionID string) *submissionEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := q.entries[submissionKey(threadID, submissionID)]
	if entry == nil {
		return nil
	}
	copy := *entry
	return &copy
}

// record inserts one entry, preserving ledger order.
func (q *submissions) record(entry *submissionEntry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := submissionKey(entry.ThreadID, entry.ID)
	if _, exists := q.entries[key]; !exists {
		q.order = append(q.order, key)
	}
	q.entries[key] = entry
	q.saveLocked()
}

func (q *submissions) saveLocked() {
	if q.path == "" {
		return
	}
	dump := map[string]*submissionEntry{}
	for _, key := range q.order {
		if entry := q.entries[key]; entry != nil {
			dump[key] = entry
		}
	}
	raw, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return
	}
	tmp := q.path + ".tmp"
	if os.WriteFile(tmp, append(raw, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, q.path)
	}
}

func (q *submissions) mutate(threadID, submissionID string, fn func(entry *submissionEntry)) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := q.entries[submissionKey(threadID, submissionID)]
	if entry == nil {
		return false
	}
	fn(entry)
	q.saveLocked()
	return true
}

func (q *submissions) setStatus(threadID, submissionID, status string) {
	q.mutate(threadID, submissionID, func(entry *submissionEntry) { entry.Status = status })
}

// requeueEntry mirrors _requeue_submission: only uncertain (unknown) entries
// climb the attempt ladder; the tenth attempt fails the entry.
func (q *submissions) requeueEntry(threadID, submissionID, message string) {
	q.mutate(threadID, submissionID, func(entry *submissionEntry) {
		if entry.Status != SubUnknown {
			return
		}
		entry.QueueAttempts++
		entry.LastQueueError = truncateSubText(message, 500)
		if entry.QueueAttempts >= maxQueueAttempts {
			entry.Status = SubFailed
		} else {
			entry.Status = SubQueued
		}
	})
}

func (q *submissions) failEntry(threadID, submissionID, message string) {
	q.mutate(threadID, submissionID, func(entry *submissionEntry) {
		if entry.Status != SubUnknown {
			return
		}
		entry.Status = SubFailed
		entry.Error = truncateSubText(message, 500)
	})
}

// meta mirrors _submission_meta: queued/unknown/failed rows in ledger order,
// shaped for the phone's renderQueue (status/text/error/attachments/editable).
// markCancelled retires a still-queued entry (goal activation turns whose
// goal command was confirmed cancelled).
func (q *submissions) markCancelled(threadID, submissionID string) {
	q.mutate(threadID, submissionID, func(entry *submissionEntry) {
		if entry.Status == SubQueued {
			entry.Status = SubCancelled
		}
	})
}

func (q *submissions) meta(threadID string) *jsoncompat.Array {
	rows := &jsoncompat.Array{}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, key := range q.order {
		entry := q.entries[key]
		if entry == nil || entry.ThreadID != threadID {
			continue
		}
		switch entry.Status {
		case SubQueued, SubUnknown, SubFailed:
		default:
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("id", entry.ID)
		row.Set("text", entry.Text)
		row.Set("status", entry.Status)
		row.Set("editable", true)
		if len(entry.AttachmentNames) > 0 {
			names := &jsoncompat.Array{}
			for _, name := range entry.AttachmentNames {
				names.Items = append(names.Items, name)
			}
			row.Set("attachments", names)
		}
		if len(entry.Attachments) > 0 {
			ids := &jsoncompat.Array{}
			for _, id := range entry.Attachments {
				ids.Items = append(ids.Items, id)
			}
			row.Set("attachmentIds", ids)
		}
		switch {
		case entry.Error != "":
			row.Set("error", entry.Error)
		case entry.LastQueueError != "":
			row.Set("error", entry.LastQueueError)
		}
		rows.Items = append(rows.Items, row)
	}
	return rows
}

// queuedThreads lists thread ids holding queued entries, first queued entry
// first per thread (the maintain loop's dispatch order).
func (q *submissions) queuedThreads() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	seen := map[string]bool{}
	threads := []string{}
	for _, key := range q.order {
		entry := q.entries[key]
		if entry == nil || entry.Status != SubQueued || seen[entry.ThreadID] {
			continue
		}
		seen[entry.ThreadID] = true
		threads = append(threads, entry.ThreadID)
	}
	return threads
}

func truncateSubText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

var errSubmissionGone = errors.New("此消息已离开队列，请查看会话")

// queueMutationError distinguishes validation failures (400) from state
// conflicts (409); both surface their Chinese message to the phone.
type queueMutationError struct{ message string }

func (e *queueMutationError) Error() string { return e.message }

func (q *submissions) editableEntry(threadID, submissionID string) (*submissionEntry, error) {
	entry := q.get(threadID, submissionID)
	if entry == nil || (entry.Status != SubQueued && entry.Status != SubFailed) {
		return nil, errSubmissionGone
	}
	return entry, nil
}

// edit mirrors edit_queued: reset the attempt ladder and queue the new text.
func (q *submissions) edit(threadID, submissionID, text string) (*submissionEntry, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len(text) > 100000 {
		return nil, &queueMutationError{"请输入 1–100000 字的消息"}
	}
	if _, err := q.editableEntry(threadID, submissionID); err != nil {
		return nil, err
	}
	ok := q.mutate(threadID, submissionID, func(entry *submissionEntry) {
		entry.Text = text
		entry.Status = SubQueued
		entry.QueueAttempts = 0
		entry.Error = ""
		entry.LastQueueError = ""
	})
	if !ok {
		return nil, errSubmissionGone
	}
	return q.get(threadID, submissionID), nil
}

// cancel mirrors cancel_queued.
func (q *submissions) cancel(threadID, submissionID string) error {
	if _, err := q.editableEntry(threadID, submissionID); err != nil {
		return err
	}
	q.setStatus(threadID, submissionID, SubCancelled)
	return nil
}

// ignore mirrors ignore_submission: only uncertain outcomes can be hidden.
func (q *submissions) ignore(threadID, submissionID string) error {
	entry := q.get(threadID, submissionID)
	if entry == nil || (entry.Status != SubUnknown && entry.Status != SubIgnored) {
		return &queueMutationError{"只能忽略发送结果未确认的记录"}
	}
	q.setStatus(threadID, submissionID, SubIgnored)
	return nil
}

// forgetThread drops every ledger entry of one thread (session delete).
func (q *submissions) forgetThread(threadID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.order[:0]
	for _, key := range q.order {
		if entry := q.entries[key]; entry != nil && entry.ThreadID == threadID {
			delete(q.entries, key)
			continue
		}
		kept = append(kept, key)
	}
	q.order = kept
	q.saveLocked()
}

// validateSubmissionInput mirrors Bridge.send's non-goal validation: the
// submission id is a UUID, the text 1–100000 chars, the mode known.
func validateSubmissionInput(submissionID, text, mode string) error {
	if _, parseErr := uuid.Parse(submissionID); parseErr != nil {
		return &queueMutationError{"消息标识无效"}
	}
	if strings.TrimSpace(text) == "" || len(text) > 100000 {
		return &queueMutationError{"请输入 1–100000 字的消息"}
	}
	switch mode {
	case "send", "steer", "queue":
		return nil
	}
	return &queueMutationError{"未知发送方式"}
}

// dispatchOutcome classifies one dispatch attempt.
type dispatchOutcome string

const (
	outcomeAccepted dispatchOutcome = "accepted"
	outcomeRequeue  dispatchOutcome = "requeue"
	outcomeFailed   dispatchOutcome = "failed"
	outcomeUnknown  dispatchOutcome = "unknown"
)

// codexThreadActive reports a mid-turn codex follower (same probe the
// activity route uses): the last turn is still inProgress.
func codexThreadActive(state *jsoncompat.Object) bool {
	if state == nil {
		return false
	}
	turns, _ := state.MustGet("turns").(*jsoncompat.Array)
	if turns == nil || len(turns.Items) == 0 {
		return false
	}
	turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	if turn == nil {
		return false
	}
	status, _ := turn.MustGet("status").(string)
	return status == "inProgress"
}

// dispatchSubmission sends one entry to its agent now and records the outcome
// on the ledger. The returned message feeds the HTTP error for failed
// dispatches; uncertain outcomes leave the entry "unknown" and return "".
func (s *Server) dispatchSubmission(threadID string, resolution acpResolution, entry *submissionEntry) (dispatchOutcome, string) {
	if resolution.manager != nil {
		return s.dispatchACPSubmission(threadID, resolution, entry)
	}
	return s.dispatchCodexSubmission(threadID, entry)
}

func (s *Server) dispatchACPSubmission(threadID string, resolution acpResolution, entry *submissionEntry) (dispatchOutcome, string) {
	manager, session := resolution.manager, resolution.session
	if session == nil {
		cwd := manager.CwdFor(threadID)
		if cwd == "" {
			cwd = "."
		}
		opened, err := manager.Open(threadID, cwd, manager.InitializeTimeout)
		if err != nil {
			// The process may be gone entirely: the prompt provably never
			// reached the agent.
			return outcomeRequeue, err.Error()
		}
		session = opened
	}
	text := entry.Text
	if len(entry.Skills) > 0 {
		name, skillErr := manager.ResolveCommandSkill(session, entry.Skills)
		if skillErr != nil {
			return outcomeFailed, skillErr.Error()
		}
		text = "/" + name + " " + text
	}
	files := []acpadapter.FileSpec{}
	for _, id := range entry.Attachments {
		file, getErr := s.Uploads.Get(threadID, id)
		if getErr != nil {
			return outcomeFailed, getErr.Error()
		}
		path, _ := file.MustGet("localPath").(string)
		name, _ := file.MustGet("name").(string)
		image, _ := file.MustGet("image").(string)
		files = append(files, acpadapter.FileSpec{Name: name, Path: path, Image: image != ""})
	}
	var extra []*jsoncompat.Object
	if len(files) > 0 {
		blocks, blockErr := acpadapter.AttachmentBlocks(session.Process.Capabilities, files)
		if blockErr != nil {
			return outcomeFailed, blockErr.Error()
		}
		for _, value := range blocks.Items {
			if block, ok := value.(*jsoncompat.Object); ok {
				extra = append(extra, block)
			}
		}
	}
	if entry.WorkMode == "plan" || entry.WorkMode == "default" {
		modeID := entry.WorkMode
		if mapped, ok := manager.ModeMap[entry.WorkMode]; ok {
			modeID = mapped
		}
		if modeErr := session.SetMode(modeID); modeErr != nil {
			return outcomeFailed, modeErr.Error()
		}
	}
	// Queued messages are phone-initiated turns too: same result-visibility
	// convention as the direct send path.
	extra = s.conventionExtras(extra)
	if promptErr := session.PromptWithBlocks(text, extra, 3600*time.Second, files...); promptErr != nil {
		if strings.Contains(promptErr.Error(), "当前已有任务正在运行") {
			// Provably not delivered: the agent refused the prompt.
			return outcomeRequeue, promptErr.Error()
		}
		return outcomeUnknown, ""
	}
	return outcomeAccepted, ""
}

func (s *Server) dispatchCodexSubmission(threadID string, entry *submissionEntry) (dispatchOutcome, string) {
	session, err := s.Codex.Watch(threadID, 1500*time.Millisecond)
	if err != nil || session == nil {
		// No desktop IPC: the turn provably never started.
		return outcomeRequeue, "请先在桌面 App 打开此聊天"
	}
	if s.Codex.Owner(session) == "" {
		return outcomeRequeue, s.Codex.ErrorOr(session, "请先在桌面 App 打开此聊天")
	}
	var skills []codex.SkillInput
	if len(entry.Skills) > 0 {
		state, _, _, _ := s.Codex.Snapshot(threadID)
		cwd, _ := state.MustGet("cwd").(string)
		resolved, skillErr := s.Codex.Catalog().ValidateSkills(cwd, entry.Skills, false)
		if skillErr != nil {
			return outcomeFailed, skillErr.Error()
		}
		for _, item := range resolved {
			skills = append(skills, codex.SkillInput{Name: item["name"], Path: item["path"]})
		}
	}
	var attachments []codex.AttachmentInput
	for _, id := range entry.Attachments {
		file, getErr := s.Uploads.Get(threadID, id)
		if getErr != nil {
			return outcomeFailed, getErr.Error()
		}
		path, _ := file.MustGet("localPath").(string)
		name, _ := file.MustGet("name").(string)
		image, _ := file.MustGet("image").(string)
		attachments = append(attachments, codex.AttachmentInput{Name: name, Path: path, Image: image != ""})
	}
	if _, callErr := s.Codex.StartTurnWithOptions(threadID, entry.ID, entry.Text, "send", entry.WorkMode, skills, attachments); callErr != nil {
		// The RPC reached the desktop; whether the turn started is unknown.
		return outcomeUnknown, ""
	}
	return outcomeAccepted, ""
}

// resolveSubmissionThread locates the owning agent of one thread for queue
// dispatch: ACP ownership first, then codex.
func (s *Server) resolveSubmissionThread(threadID string) acpResolution {
	if s.Codex == nil && len(s.acpAgentSet()) == 0 {
		return acpResolution{}
	}
	return s.resolveACP(threadID)
}

// threadConnected reports whether a dispatch could reach the agent now.
func (s *Server) threadConnected(threadID string, resolution acpResolution) bool {
	if resolution.manager != nil {
		return true // resolved ownership implies an openable session
	}
	if s.Codex == nil {
		return false
	}
	session, err := s.Codex.Watch(threadID, 1500*time.Millisecond)
	return err == nil && session != nil && s.Codex.Owner(session) != ""
}

// dispatchQueuedThread sends the thread's first queued entry when the agent
// is connected and idle. settle names the ACP kind when invoked from the
// turn-settle hook (its session handle is reused instead of reopening).
func (s *Server) dispatchQueuedThread(threadID, kind string, session *acpruntime.Session) {
	if s.Queue == nil {
		return
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	var first *submissionEntry
	for _, entry := range s.Queue.threadEntries(threadID) {
		if entry.Status == SubQueued {
			first = entry
			break
		}
	}
	if first == nil {
		return
	}
	resolution := s.resolveSubmissionThread(threadID)
	if kind != "" && session != nil {
		resolution = acpResolution{kind: kind, manager: resolution.manager, session: session}
	}
	if !s.threadConnected(threadID, resolution) {
		return
	}
	if s.threadActive(threadID, resolution) {
		return
	}
	s.Queue.setStatus(threadID, first.ID, SubUnknown)
	outcome, message := s.dispatchSubmission(threadID, resolution, first)
	switch outcome {
	case outcomeAccepted:
		s.Queue.setStatus(threadID, first.ID, SubAccepted)
	case outcomeRequeue:
		s.Queue.requeueEntry(threadID, first.ID, message)
	case outcomeFailed:
		s.Queue.failEntry(threadID, first.ID, message)
	default: // uncertain: stays unknown, never automatically replayed
	}
}

// threadActive reports a mid-turn agent for either runtime.
func (s *Server) threadActive(threadID string, resolution acpResolution) bool {
	if resolution.manager != nil {
		session := resolution.session
		if session == nil {
			session = resolution.manager.Get(threadID)
		}
		if session == nil {
			return false
		}
		return session.Snapshot().MustGet("status") == "active"
	}
	state, _, _, _ := s.Codex.Snapshot(threadID)
	return codexThreadActive(state)
}

// maintainLoop 是后台维护循环的启停句柄（§12 阶段四：给裸 ticker goroutine
// 补停止通道——stop chan + sync.Once 幂等关闭照 discovery.close 的先例，再加
// done 通道让 close 同步等待在途一轮收尾）。body 在循环 goroutine 内同步
// 执行；close 期间至多等完一轮在途 body。
type maintainLoop struct {
	stop     chan struct{}
	once     sync.Once
	done     chan struct{}
	interval time.Duration
	body     func()
}

// newMaintainLoop 启动一个每 interval 执行一次 body 的维护循环。
func newMaintainLoop(interval time.Duration, body func()) *maintainLoop {
	loop := &maintainLoop{
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		interval: interval,
		body:     body,
	}
	go loop.run()
	return loop
}

func (l *maintainLoop) run() {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.body()
		}
	}
}

// close 停止循环并同步等待退出（幂等；nil 接收者无副作用——未启动的循环
// 可安全停止）。
func (l *maintainLoop) close() {
	if l == nil {
		return
	}
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

// StartQueueWorker runs the maintain loop's dispatch pass: every 3s each
// thread holding queued entries gets one dispatch attempt while its agent is
// connected and idle. StopQueueWorker 停止循环（幂等，同步等待在途派发轮
// 收尾）；重复调用无副作用。
func (s *Server) StartQueueWorker() {
	if s.Queue == nil || s.queueWorker != nil {
		return
	}
	s.queueWorker = newMaintainLoop(3*time.Second, s.safeDispatchQueuedPass)
}

// StopQueueWorker 停止排队派发循环（幂等；停止后可重新 Start）。
func (s *Server) StopQueueWorker() {
	if loop := s.queueWorker; loop != nil {
		s.queueWorker = nil
		loop.close()
	}
}

// StartIdleSweeper 每 30s 清扫 5 分钟无人读的 ACP 会话（main.go 原内联
// goroutine 的 Server 化：Idle ACP sessions hold one child process each——
// 照 Python maintain loop 的 300s eviction once nobody is reading them）。
// StopIdleSweeper 停止循环；重复调用无副作用。
func (s *Server) StartIdleSweeper() {
	if s.idleSweeper != nil {
		return
	}
	s.idleSweeper = newMaintainLoop(30*time.Second, func() {
		for _, manager := range s.ACPAgents {
			if manager != nil {
				manager.SweepIdle(5 * time.Minute)
			}
		}
	})
}

// StopIdleSweeper 停止空闲会话清扫循环（幂等；停止后可重新 Start）。
func (s *Server) StopIdleSweeper() {
	if loop := s.idleSweeper; loop != nil {
		s.idleSweeper = nil
		loop.close()
	}
}

// safeDispatchQueuedPass keeps one bad dispatch from killing the gateway: a
// panic in this background loop is process-fatal, so it is recovered and
// logged — the next tick retries whatever is still queued.
func (s *Server) safeDispatchQueuedPass() {
	defer func() {
		if problem := recover(); problem != nil {
			log.Printf("排队派发异常（已恢复，下个周期重试）: %v\n%s", problem, debug.Stack())
		}
	}()
	s.dispatchQueuedPass()
}

func (s *Server) dispatchQueuedPass() {
	for _, threadID := range s.Queue.queuedThreads() {
		s.dispatchQueuedThread(threadID, "", nil)
	}
}

// QueueSettleACP is the turn-settle hook (Manager.AutoContinue chain): the
// moment a live ACP turn ends, any queued message dispatches immediately.
func (s *Server) QueueSettleACP(kind string, session *acpruntime.Session) {
	if s.Queue == nil || session == nil {
		return
	}
	s.dispatchQueuedThread(session.ID, kind, session)
}

// threadEntries returns copies of one thread's ledger entries in order.
func (q *submissions) threadEntries(threadID string) []*submissionEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := []*submissionEntry{}
	for _, key := range q.order {
		entry := q.entries[key]
		if entry == nil || entry.ThreadID != threadID {
			continue
		}
		copy := *entry
		entries = append(entries, &copy)
	}
	return entries
}

// enqueue validates and records one send; a same-id same-content retry
// returns the prior entry with duplicate=true (Python send's idempotency).
func (q *submissions) enqueue(threadID, submissionID, text, mode, workMode string,
	skills, attachments, attachmentNames []string) (*submissionEntry, bool, error) {
	if err := validateSubmissionInput(submissionID, text, mode); err != nil {
		return nil, false, err
	}
	if prior := q.get(threadID, submissionID); prior != nil {
		same := prior.Text == text && prior.Mode == mode && prior.WorkMode == workMode &&
			strings.Join(prior.Skills, "\x00") == strings.Join(skills, "\x00") &&
			strings.Join(prior.Attachments, "\x00") == strings.Join(attachments, "\x00")
		if !same {
			return nil, false, &queueMutationError{"同一消息标识不能用于不同内容"}
		}
		return prior, true, nil
	}
	entry := &submissionEntry{ID: submissionID, ThreadID: threadID, Host: "local",
		Text: text, Mode: mode, Skills: skills, WorkMode: workMode,
		Attachments: attachments, AttachmentNames: attachmentNames,
		Status: SubQueued, CreatedAt: float64(time.Now().UnixMilli()) / 1000}
	if mode != "queue" {
		entry.Status = SubUnknown
	}
	q.record(entry)
	return q.get(threadID, submissionID), false, nil
}

// submissionPayload is the phone composer's send body subset the ledger needs.
type submissionPayload struct {
	id          string
	text        string
	mode        string
	workMode    string
	skills      []string
	attachments []string
	uiLocale    string
}

// readSubmissionPayload extracts the composer fields shared by the codex and
// ACP send routes.
func readSubmissionPayload(body *jsoncompat.Object) submissionPayload {
	payload := submissionPayload{
		id:       stringField(body, "id"),
		text:     stringField(body, "text"),
		mode:     stringField(body, "mode"),
		workMode: stringField(body, "workMode"),
		skills:   bodyList(body, "skills"),
		uiLocale: stringField(body, "uiLocale"),
	}
	// Python's send defaults mode="send"; an omitted mode is a plain send.
	if payload.mode == "" {
		payload.mode = "send"
	}
	if raw, ok := body.MustGet("attachments").(*jsoncompat.Array); ok {
		for _, value := range raw.Items {
			if entry, ok := value.(*jsoncompat.Object); ok {
				if id, _ := entry.MustGet("id").(string); id != "" {
					payload.attachments = append(payload.attachments, id)
				}
			} else if legacyID, ok := value.(string); ok && legacyID != "" {
				payload.attachments = append(payload.attachments, legacyID)
			}
		}
	}
	return payload
}

// resolveAttachmentNames maps upload ids to stored names for the queue card
// (attachmentNames in _submission_meta). Missing uploads keep the raw id so
// the dispatch-time failure still lists something recognizable.
func (h *handler) resolveAttachmentNames(threadID string, ids []string) []string {
	if h.server.Uploads == nil {
		return ids
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		name := id
		if file, err := h.server.Uploads.Get(threadID, id); err == nil {
			if stored, _ := file.MustGet("name").(string); stored != "" {
				name = stored
			}
		}
		names = append(names, name)
	}
	return names
}

// handleQueuedSend serves the send route's mode=="queue" branch for both
// runtimes: the entry joins the ledger and dispatches when the turn settles.
func (h *handler) handleQueuedSend(threadID string, body *jsoncompat.Object) {
	if h.server.Queue == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用排队发送"))
		return
	}
	payload := readSubmissionPayload(body)
	if payload.mode == "steer" && payload.workMode != "" {
		h.fail(http.StatusBadRequest, errorBody("补充当前任务时不能切换工作模式"))
		return
	}
	names := h.resolveAttachmentNames(threadID, payload.attachments)
	entry, duplicate, err := h.server.Queue.enqueue(threadID, payload.id, payload.text,
		payload.mode, payload.workMode, payload.skills, payload.attachments, names)
	if err != nil {
		status := http.StatusBadRequest
		if err == errSubmissionGone {
			status = http.StatusConflict
		}
		h.fail(status, errorBody(err.Error()))
		return
	}
	result := jsoncompat.NewObject()
	result.Set("status", entry.Status)
	result.Set("id", entry.ID)
	if duplicate {
		result.Set("duplicate", true)
	}
	// An idle agent picks the message up within the maintain pass; a running
	// turn dispatches through the settle hook (Python _maintain semantics —
	// no eager dispatch here, so enqueue ordering stays authoritative).
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// recordDirectSend wraps a direct (non-queue) send in the ledger so retries
// stay idempotent and uncertain outcomes reach the phone's queue cards.
// Returns (duplicate, priorStatus, ok).
func (h *handler) recordDirectSend(threadID string, payload submissionPayload) (bool, *submissionEntry, error) {
	if h.server.Queue == nil {
		return false, nil, nil
	}
	names := h.resolveAttachmentNames(threadID, payload.attachments)
	entry, duplicate, err := h.server.Queue.enqueue(threadID, payload.id, payload.text,
		payload.mode, payload.workMode, payload.skills, payload.attachments, names)
	if err != nil {
		return false, nil, err
	}
	if duplicate {
		return true, entry, nil
	}
	return false, entry, nil
}

// handleQueueRoute serves POST /api/sessions/<id>/queue {id, action, text}:
// send-now / edit / delete over the ledger, agent kind resolved here so both
// runtimes share one route.
func (h *handler) handleQueueRoute(threadID string, body *jsoncompat.Object) {
	if h.server.Queue == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用排队发送"))
		return
	}
	submissionID := stringField(body, "id")
	if _, parseErr := uuid.Parse(submissionID); parseErr != nil {
		h.fail(http.StatusBadRequest, errorBody("消息标识无效"))
		return
	}
	switch stringField(body, "action") {
	case "delete":
		if err := h.server.Queue.cancel(threadID, submissionID); err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return
		}
		result := jsoncompat.NewObject()
		result.Set("status", SubCancelled)
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case "edit":
		entry, err := h.server.Queue.edit(threadID, submissionID, stringField(body, "text"))
		if err != nil {
			status := http.StatusBadRequest
			if err == errSubmissionGone {
				status = http.StatusConflict
			}
			h.fail(status, errorBody(err.Error()))
			return
		}
		result := jsoncompat.NewObject()
		result.Set("status", entry.Status)
		result.Set("id", entry.ID)
		result.Set("text", entry.Text)
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case "send":
		h.queueSendNow(threadID, submissionID)
	default:
		h.fail(http.StatusBadRequest, errorBody("未知队列操作"))
	}
}

// interruptSubmissionTurn stops the active turn before a manual send-now.
func (s *Server) interruptSubmissionTurn(threadID string, resolution acpResolution) error {
	if resolution.manager != nil {
		session := resolution.session
		if session == nil {
			session = resolution.manager.Get(threadID)
		}
		if session == nil {
			return nil
		}
		return session.CancelAndWait(10 * time.Second)
	}
	_, err := s.Codex.Interrupt(threadID)
	return err
}

// queueSendNow stops any active turn, then dispatches the selected entry.
// The queue lock keeps the cancellation's settle hook from dispatching another
// entry between those two steps.
func (h *handler) queueSendNow(threadID, submissionID string) {
	h.server.queueMu.Lock()
	defer h.server.queueMu.Unlock()

	entry, err := h.server.Queue.editableEntry(threadID, submissionID)
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	resolution := h.server.resolveSubmissionThread(threadID)
	if !h.server.threadConnected(threadID, resolution) {
		message := "会话尚未连接，请重新打开"
		if resolution.manager == nil && h.server.Codex != nil {
			if session, watchErr := h.server.Codex.Watch(threadID, 1500*time.Millisecond); watchErr == nil && session != nil {
				message = h.server.Codex.ErrorOr(session, message)
			}
		}
		h.fail(http.StatusConflict, errorBody(message))
		return
	}
	if h.server.threadActive(threadID, resolution) {
		if err := h.server.interruptSubmissionTurn(threadID, resolution); err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return
		}
	}
	h.server.Queue.setStatus(threadID, submissionID, SubUnknown)
	h.server.Queue.mutate(threadID, submissionID, func(entry *submissionEntry) {
		entry.QueueAttempts = 0
		entry.Error = ""
		entry.LastQueueError = ""
	})
	outcome, message := h.server.dispatchSubmission(threadID, resolution, entry)
	switch outcome {
	case outcomeAccepted:
		h.server.Queue.setStatus(threadID, submissionID, SubAccepted)
		result := jsoncompat.NewObject()
		result.Set("status", SubAccepted)
		result.Set("id", submissionID)
		result.Set("result", jsoncompat.NewObject())
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case outcomeUnknown:
		result := jsoncompat.NewObject()
		result.Set("status", SubUnknown)
		result.Set("id", submissionID)
		h.output(http.StatusOK, result, contentTypeJSON, "")
	default:
		if outcome == outcomeRequeue {
			h.server.Queue.requeueEntry(threadID, submissionID, message)
		} else {
			h.server.Queue.failEntry(threadID, submissionID, message)
		}
		h.fail(http.StatusConflict, errorBody(message))
	}
}

// submissionDuplicateResponse shapes a retried send: the prior status and no
// side effects (Python send's duplicate branch).
func submissionDuplicateResponse(entry *submissionEntry) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("status", entry.Status)
	result.Set("duplicate", true)
	result.Set("id", entry.ID)
	return result
}
