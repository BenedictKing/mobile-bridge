package store

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// remoteHelper is the read-only script RemoteStore ships to another computer
// over stdin. It is deliberately self-contained: nothing is installed on the
// remote host, and the request it answers travels inside the script itself.
//
//go:embed remote_helper.py
var remoteHelper string

var (
	// ErrRemoteUnavailable reports a host that could not be read at all:
	// invalid alias, failed or timed-out connection, unreadable reply.
	ErrRemoteUnavailable = errors.New("SSH 主机暂不可用，请检查电脑上的连接")
	// ErrRemotePython reports a host where `python3` cannot be started.
	ErrRemotePython = errors.New("远端主机缺少 Python 3，无法读取 SSH 会话")
)

const (
	// DefaultRemoteTimeout bounds one helper round trip.
	DefaultRemoteTimeout = 18 * time.Second
	remoteListTTL        = 15 * time.Second
	remoteHistoryTTL     = 20 * time.Second
	remoteHistorySlots   = 8
	remoteListSlots      = 16
	remoteOutputCap      = 64 << 20
)

// RemoteRunner ships a helper script to a host through its SSH alias and
// returns what the script printed. Tests substitute a fake.
type RemoteRunner func(ctx context.Context, alias, script string) ([]byte, error)

// ValidAlias mirrors bridge/remote.py's ssh_read guard: an alias is one word
// from the user's SSH configuration, never an option or a command.
func ValidAlias(alias string) bool {
	return alias != "" && !strings.HasPrefix(alias, "-") && !strings.ContainsAny(alias, " \t\r\n")
}

// limitedWriter keeps a runaway remote reply from growing memory without
// bounds; everything past the cap is dropped.
type limitedWriter struct {
	writer    *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return len(data), nil
	}
	kept := data
	if len(kept) > w.remaining {
		kept = kept[:w.remaining]
	}
	w.remaining -= len(kept)
	_, err := w.writer.Write(kept)
	return len(data), err
}

// SSHRunner is the production RemoteRunner: the non-interactive, strictly
// key-checked, forwarding-free ssh invocation bridge/remote.py used, feeding
// the script to a remote python3 over stdin. Passwords and confirmations are
// never prompted for, so a host that needs them simply reports unavailable.
func SSHRunner(ctx context.Context, alias, script string) ([]byte, error) {
	command := exec.CommandContext(ctx, "ssh", "-T",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UpdateHostKeys=no",
		alias, "python3 -")
	command.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	command.Stdout = &limitedWriter{writer: &stdout, remaining: remoteOutputCap}
	command.Stderr = &limitedWriter{writer: &stderr, remaining: 4096}
	err := command.Run()
	if ctx.Err() != nil {
		return nil, ErrRemoteUnavailable
	}
	if err == nil {
		return stdout.Bytes(), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 127 {
		return nil, ErrRemotePython
	}
	log.Printf("SSH 远端读取失败（alias=%s）: %v %s", alias, err, strings.TrimSpace(stderr.String()))
	return nil, ErrRemoteUnavailable
}

type remoteListCache struct {
	at   time.Time
	rows []*jsoncompat.Object
}

type remoteHistoryCache struct {
	at        time.Time
	meta      *jsoncompat.Object
	content   string
	truncated bool
}

// RemoteStore reads the Codex session index and rollouts of another computer
// through an SSH alias the desktop App already saved (bridge/remote.py's
// RemoteStore). The helper only runs the query listPlan produced and returns
// rollout bytes; parsing stays in this process, so local and remote histories
// are read by the same code and no remote path is ever mapped locally.
// Lists and histories are cached briefly because every miss is an SSH round
// trip; cached reads also keep timeline cursors stable between requests.
type RemoteStore struct {
	Alias   string
	Run     RemoteRunner
	Timeout time.Duration

	mu        sync.Mutex
	columns   map[string]bool
	lists     map[string]remoteListCache
	histories map[string]remoteHistoryCache
}

func NewRemoteStore(alias string, run RemoteRunner) *RemoteStore {
	if run == nil {
		run = SSHRunner
	}
	return &RemoteStore{Alias: alias, Run: run,
		lists:     map[string]remoteListCache{},
		histories: map[string]remoteHistoryCache{}}
}

func (r *RemoteStore) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultRemoteTimeout
}

// call answers one helper request, turning the helper's own refusals into
// plain errors and anything unreadable into ErrRemoteUnavailable.
func (r *RemoteStore) call(ctx context.Context, request *jsoncompat.Object) (*jsoncompat.Object, error) {
	if !ValidAlias(r.Alias) {
		return nil, errors.New("SSH 别名无效")
	}
	encoded := strconv.Quote(base64.StdEncoding.EncodeToString(jsoncompat.EncodeCompact(request)))
	script := remoteHelper + "\nimport sys\ntry:\n    _reply = handle(json.loads(base64.b64decode(" + encoded + ")))\n" +
		"except Fail as _failure:\n    _reply = {\"error\": str(_failure)}\n" +
		"sys.stdout.write(json.dumps(_reply, ensure_ascii=False))\n"
	output, err := r.Run(ctx, r.Alias, script)
	if err != nil {
		return nil, err
	}
	value, err := jsoncompat.Unmarshal(lastLine(output))
	if err != nil {
		return nil, ErrRemoteUnavailable
	}
	result, _ := value.(*jsoncompat.Object)
	if result == nil {
		return nil, ErrRemoteUnavailable
	}
	if failure, ok := result.MustGet("error").(string); ok && failure != "" {
		return nil, errors.New(failure)
	}
	return result, nil
}

// lastLine keeps the helper's single-line reply even when the remote shell
// prints a banner first.
func lastLine(output []byte) []byte {
	text := bytes.TrimSpace(output)
	if index := bytes.LastIndexByte(text, '\n'); index >= 0 {
		text = bytes.TrimSpace(text[index+1:])
	}
	return text
}

func (r *RemoteStore) threadColumns(ctx context.Context) (map[string]bool, error) {
	r.mu.Lock()
	columns := r.columns
	r.mu.Unlock()
	if columns != nil {
		return columns, nil
	}
	request := jsoncompat.NewObject()
	request.Set("op", "schema")
	result, err := r.call(ctx, request)
	if err != nil {
		return nil, err
	}
	names, _ := result.MustGet("columns").(*jsoncompat.Array)
	if names == nil {
		return nil, ErrRemoteUnavailable
	}
	columns = map[string]bool{}
	for _, name := range names.Items {
		if text, ok := name.(string); ok {
			columns[text] = true
		}
	}
	r.mu.Lock()
	r.columns = columns
	r.mu.Unlock()
	return columns, nil
}

// List mirrors SessionStore.List on the remote index, in recency order.
func (r *RemoteStore) List(query string, limit, offset int, archived bool) ([]*jsoncompat.Object, error) {
	key := fmt.Sprintf("%q|%d|%d|%v", query, limit, offset, archived)
	r.mu.Lock()
	cached, fresh := r.lists[key]
	r.mu.Unlock()
	if fresh && time.Since(cached.at) < remoteListTTL {
		return cloneRows(cached.rows), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout())
	defer cancel()
	columns, err := r.threadColumns(ctx)
	if err != nil {
		return nil, err
	}
	fields, statement, args, err := listPlan(columns, query, limit, offset, archived)
	if err != nil {
		return nil, err
	}
	request := jsoncompat.NewObject()
	request.Set("op", "query")
	request.Set("sql", statement)
	values := &jsoncompat.Array{}
	for _, arg := range args {
		values.Items = append(values.Items, arg)
	}
	request.Set("args", values)
	result, err := r.call(ctx, request)
	if err != nil {
		return nil, err
	}
	rows, _ := result.MustGet("rows").(*jsoncompat.Array)
	if rows == nil {
		return nil, ErrRemoteUnavailable
	}
	objects := make([]*jsoncompat.Object, 0, len(rows.Items))
	for _, item := range rows.Items {
		values, ok := item.(*jsoncompat.Array)
		if !ok || len(values.Items) != len(fields) {
			return nil, ErrRemoteUnavailable
		}
		row := jsoncompat.NewObject()
		for index, field := range fields {
			row.Set(field, sqlValue(values.Items[index]))
		}
		objects = append(objects, row)
	}
	r.mu.Lock()
	if len(r.lists) >= remoteListSlots {
		r.lists = map[string]remoteListCache{}
	}
	r.lists[key] = remoteListCache{at: time.Now(), rows: objects}
	r.mu.Unlock()
	return cloneRows(objects), nil
}

// cloneRows hands every caller its own rows: list handlers decorate rows in
// place, and cached objects are shared between requests.
func cloneRows(rows []*jsoncompat.Object) []*jsoncompat.Object {
	copies := make([]*jsoncompat.Object, 0, len(rows))
	for _, row := range rows {
		if copy, ok := jsoncompat.Clone(row).(*jsoncompat.Object); ok {
			copies = append(copies, copy)
		}
	}
	return copies
}

// HistoryFor mirrors SessionStore.HistoryFor: the remote helper returns the
// thread row and the newest rollout bytes, and this process parses them. A
// rollout past the helper's cap arrives without its oldest turns, which the
// result reports through turnsPagination like any partial history.
func (r *RemoteStore) HistoryFor(threadID string, turnLimit *int) (*jsoncompat.Object, error) {
	r.mu.Lock()
	cached, fresh := r.histories[threadID]
	r.mu.Unlock()
	if !fresh || time.Since(cached.at) >= remoteHistoryTTL {
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout())
		defer cancel()
		request := jsoncompat.NewObject()
		request.Set("op", "rollout")
		request.Set("thread_id", threadID)
		result, err := r.call(ctx, request)
		if err != nil {
			return nil, err
		}
		row, _ := result.MustGet("row").(*jsoncompat.Object)
		if row == nil {
			return nil, ErrRemoteUnavailable
		}
		meta, err := userFacingRow(row)
		if err != nil {
			return nil, err
		}
		content, _ := result.MustGet("content").(string)
		truncated, _ := result.MustGet("truncated").(bool)
		cached = remoteHistoryCache{at: time.Now(), meta: meta, content: content, truncated: truncated}
		r.mu.Lock()
		if len(r.histories) >= remoteHistorySlots {
			r.histories = map[string]remoteHistoryCache{}
		}
		r.histories[threadID] = cached
		r.mu.Unlock()
	}
	history := History(threadID, cached.meta, []byte(cached.content), turnLimit)
	if cached.truncated {
		if pagination, ok := history.MustGet("turnsPagination").(*jsoncompat.Object); ok {
			pagination.Set("hasLoadedOldest", false)
		}
	}
	return history, nil
}
