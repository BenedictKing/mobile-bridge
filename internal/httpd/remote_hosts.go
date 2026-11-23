package httpd

// SSH 远端会话（网关基准 §3.3）：主机与项目来自桌面 App 保存的连接，
// 列表与历史经 store.RemoteStore 只读读取；网关没有远端运行 owner，
// 所有写操作明确拒绝。远端读取只由请求触发，后台任务从不跨 SSH。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

const (
	// remoteReadOnlyHint is shown in place of the composer on SSH sessions.
	remoteReadOnlyHint = "SSH 远端会话目前只能查看；发送等操作请在电脑 App 中进行"
	// remoteFailureTTL keeps a host that just failed out of SSH for a while,
	// so one unreachable host cannot stall every list request.
	remoteFailureTTL = time.Minute
	remoteWait       = 12 * time.Second
)

var errRemoteReadOnly = errors.New(remoteReadOnlyHint)

// remoteHost is one saved entry of codex-managed-remote-connections.
type remoteHost struct {
	ID    string
	Alias string
	Label string
}

// remoteProject is one saved entry of remote-projects.
type remoteProject struct {
	ID     string
	HostID string
	Path   string
	Label  string
}

func (p remoteProject) name() string {
	if p.Label != "" {
		return p.Label
	}
	return projectBaseName(p.Path)
}

// appHosts mirrors bridge/remote.py's AppHosts: saved SSH connections that own
// at least one remote project or session, their projects, and the thread
// assignments that place a remote session in one of them.
type appHosts struct {
	order       []string
	hosts       map[string]remoteHost
	projects    []remoteProject
	assignments map[string][2]string // session id -> {projectId, hostId}
}

func readAppHosts(codexHome string) *appHosts {
	result := &appHosts{hosts: map[string]remoteHost{}, assignments: map[string][2]string{}}
	root := readGlobalState(codexHome)
	if root == nil {
		return result
	}
	relevant := map[string]bool{}
	projects := []remoteProject{}
	if list, ok := root.MustGet("remote-projects").(*jsoncompat.Array); ok {
		for _, item := range list.Items {
			entry, _ := item.(*jsoncompat.Object)
			if entry == nil {
				continue
			}
			project := remoteProject{ID: stringField(entry, "id"), HostID: stringField(entry, "hostId"),
				Path: stringField(entry, "remotePath"), Label: stringField(entry, "label")}
			if project.HostID == "" {
				continue
			}
			relevant[project.HostID] = true
			if project.ID != "" && project.Path != "" {
				projects = append(projects, project)
			}
		}
	}
	if members, ok := root.MustGet("thread-project-membership-host-ids").(*jsoncompat.Object); ok {
		for _, key := range members.Keys() {
			if id, _ := members.MustGet(key).(string); id != "" {
				relevant[id] = true
			}
		}
	}
	if list, ok := root.MustGet("codex-managed-remote-connections").(*jsoncompat.Array); ok {
		for _, item := range list.Items {
			entry, _ := item.(*jsoncompat.Object)
			if entry == nil {
				continue
			}
			id, alias := stringField(entry, "hostId"), stringField(entry, "alias")
			if id == "" || id == "local" || alias == "" || !relevant[id] {
				continue
			}
			if _, seen := result.hosts[id]; seen {
				continue
			}
			label := stringField(entry, "displayName")
			if label == "" {
				label = alias
			}
			result.hosts[id] = remoteHost{ID: id, Alias: alias, Label: label}
			result.order = append(result.order, id)
		}
	}
	for _, project := range projects {
		if _, ok := result.hosts[project.HostID]; ok {
			result.projects = append(result.projects, project)
		}
	}
	if items, ok := root.MustGet("thread-project-assignments").(*jsoncompat.Object); ok {
		for _, sessionID := range items.Keys() {
			if entry, _ := items.MustGet(sessionID).(*jsoncompat.Object); entry != nil {
				result.assignments[sessionID] = [2]string{stringField(entry, "projectId"), stringField(entry, "hostId")}
			}
		}
	}
	return result
}

func (a *appHosts) host(id string) (remoteHost, bool) {
	host, ok := a.hosts[id]
	return host, ok
}

// projectFor mirrors AppHosts.decorate for one remote row: an assignment
// naming this host (or no host) wins, else the longest saved remote path that
// is the cwd or a prefix of it.
func (a *appHosts) projectFor(hostID, sessionID, cwd string) (remoteProject, bool) {
	assigned := a.assignments[sessionID]
	if assigned[0] != "" && (assigned[1] == "" || assigned[1] == hostID) {
		for _, project := range a.projects {
			if project.HostID == hostID && project.ID == assigned[0] {
				return project, true
			}
		}
	}
	normalized := strings.TrimRight(cwd, "/")
	var best remoteProject
	bestLen := -1
	for _, project := range a.projects {
		root := strings.TrimRight(project.Path, "/")
		if project.HostID != hostID || root == "" {
			continue
		}
		if (normalized == root || strings.HasPrefix(normalized, root+"/")) && len(root) > bestLen {
			best, bestLen = project, len(root)
		}
	}
	return best, bestLen >= 0
}

// decorateRemoteRow gives a remote codex row the list contract fields. The
// project and group keys start with the host id, so remote sessions never
// merge with a local or another host's directory of the same name.
func (a *appHosts) decorateRemoteRow(host remoteHost, row *jsoncompat.Object) {
	cwd := stringField(row, "cwd")
	key, name := strings.TrimRight(cwd, "/"), projectBaseName(cwd)
	if project, ok := a.projectFor(host.ID, stringField(row, "id"), cwd); ok {
		key, name = project.ID, project.name()
	}
	if key == "" {
		key = "unassigned"
	}
	if name == "" {
		name = "未归类"
	}
	row.Set("title", codexListTitle(row))
	row.Set("host", host.ID)
	row.Set("hostLabel", host.Label)
	row.Set("agent", "codex")
	row.Set("projectKey", host.ID+"|"+key)
	row.Set("projectName", name)
	row.Set("groupKey", host.ID+"|"+key)
	row.Set("groupName", name)
	row.Set("recency", recencyOf(row))
	row.Set("connected", false)
}

// projectEntries lists saved remote projects for /api/projects. Their key is
// the same host|project identity remote rows carry as projectKey.
func (a *appHosts) projectEntries() []*projectEntry {
	entries := make([]*projectEntry, 0, len(a.projects))
	for _, project := range a.projects {
		host := a.hosts[project.HostID]
		entry := newProjectEntry(project.HostID+"|"+project.ID, project.Path, "codex", 0)
		entry.item.Set("host", host.ID)
		entry.item.Set("hostLabel", host.Label)
		entry.item.Set("name", project.name())
		entries = append(entries, entry)
	}
	return entries
}

func (a *appHosts) isProjectKey(key string) bool {
	for _, project := range a.projects {
		if project.HostID+"|"+project.ID == key {
			return true
		}
	}
	return false
}

type remoteFailure struct {
	at  time.Time
	err error
}

// remoteStore returns the host's reader, replacing it when the saved alias
// changed. Readers keep their own short caches between requests.
func (s *Server) remoteStore(host remoteHost) *store.RemoteStore {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	if s.remoteStores == nil {
		s.remoteStores = map[string]*store.RemoteStore{}
	}
	reader := s.remoteStores[host.ID]
	if reader == nil || reader.Alias != host.Alias {
		reader = store.NewRemoteStore(host.Alias, s.remoteRun)
		s.remoteStores[host.ID] = reader
	}
	return reader
}

// remoteRead runs one list read with the per-host failure backoff.
func (s *Server) remoteRead(host remoteHost, read func(*store.RemoteStore) error) error {
	s.remoteMu.Lock()
	failure, failed := s.remoteFailed[host.ID]
	s.remoteMu.Unlock()
	if failed && time.Since(failure.at) < remoteFailureTTL {
		return failure.err
	}
	err := read(s.remoteStore(host))
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	if s.remoteFailed == nil {
		s.remoteFailed = map[string]remoteFailure{}
	}
	if err != nil {
		s.remoteFailed[host.ID] = remoteFailure{at: time.Now(), err: err}
	} else {
		delete(s.remoteFailed, host.ID)
	}
	return err
}

// remoteSessionRows reads every saved host in parallel. A host that fails is
// listed in unavailable with its reason; the others are unaffected.
func (s *Server) remoteSessionRows(text string, limit int, archived bool) (rows, unavailable []*jsoncompat.Object) {
	hosts := readAppHosts(s.CodexHome)
	results := make([][]*jsoncompat.Object, len(hosts.order))
	failures := make([]error, len(hosts.order))
	var group sync.WaitGroup
	for index, id := range hosts.order {
		group.Add(1)
		go func(index int, host remoteHost) {
			defer group.Done()
			failures[index] = s.remoteRead(host, func(reader *store.RemoteStore) error {
				list, err := reader.List(text, limit, 0, archived)
				results[index] = list
				return err
			})
		}(index, hosts.hosts[id])
	}
	group.Wait()
	for index, id := range hosts.order {
		host := hosts.hosts[id]
		if failures[index] != nil {
			entry := jsoncompat.NewObject()
			entry.Set("host", host.ID)
			entry.Set("label", host.Label)
			entry.Set("error", failures[index].Error())
			unavailable = append(unavailable, entry)
			continue
		}
		for _, row := range results[index] {
			if s.hiddenSession(stringField(row, "id")) {
				continue
			}
			hosts.decorateRemoteRow(host, row)
			rows = append(rows, row)
		}
	}
	return rows, unavailable
}

// remoteView reads a remote thread's history and shapes it like a saved
// local history view, flagged read-only with the reason.
func (s *Server) remoteView(host remoteHost, threadID string) (*jsoncompat.Object, error) {
	if s.hiddenSession(threadID) {
		return nil, errors.New("找不到这个会话")
	}
	history, err := s.remoteStore(host).HistoryFor(threadID, nil)
	if err != nil {
		return nil, err
	}
	view := buildHistoryView(history)
	view.Set("host", host.ID)
	view.Set("hostLabel", host.Label)
	view.Set("agent", "codex")
	view.Set("capabilities", &jsoncompat.Array{})
	view.Set("readOnly", true)
	view.Set("readOnlyHint", remoteReadOnlyHint)
	view.Set("connected", false)
	view.Set("connecting", false)
	view.Set("activating", false)
	view.Set("loadingHistory", false)
	return view, nil
}

// withRemoteProjection folds the remote view into a projection slot scoped
// by host (thread route ids cannot contain "|", so it never collides with a
// local thread) and runs read under the slot lock.
func (s *Server) withRemoteProjection(host remoteHost, threadID string,
	read func(*timeline.Timeline) (*jsoncompat.Object, error)) (*jsoncompat.Object, error) {
	view, err := s.remoteView(host, threadID)
	if err != nil {
		return nil, err
	}
	slot := s.projectionSlotFor("ssh|" + host.ID + "|" + threadID)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	projection := slot.timeline
	projection.Update(view)
	projection.SetMeta("host", host.ID)
	projection.SetMeta("hostLabel", host.Label)
	projection.SetMeta("agent", "codex")
	projection.SetMeta("capabilities", &jsoncompat.Array{})
	if index := projection.UserIndex(); len(index.Items) > 0 {
		projection.SetMeta("userIndex", index)
	}
	return read(projection)
}

// handleRemoteSessionRoute serves session routes for a non-local host. It
// returns false for paths that are not per-session routes.
func (h *handler) handleRemoteSessionRoute(hostID string, write bool, path string, query map[string][]string) bool {
	threadID, action := "", ""
	if match := goalRoute.FindStringSubmatch(path); match != nil {
		threadID, action = match[1], "goal"
	} else if match := threadRoute.FindStringSubmatch(path); match != nil {
		threadID, action = match[1], match[2]
	} else {
		return false
	}
	host, ok := readAppHosts(h.server.CodexHome).host(hostID)
	if !ok {
		h.fail(http.StatusNotFound, errorBody("找不到这个会话"))
		return true
	}
	// Notification policy and read marks are local ledgers keyed by host.
	if action == "notifications" || (write && action == "read") {
		bridge, err := h.server.Bridge.ForHost("local")
		if err != nil {
			h.fail(http.StatusNotFound, errorBody("找不到这个会话"))
			return true
		}
		return h.handleSessionRoutes(bridge, write, path, query)
	}
	// Opening any chat asks its owner to attach; a remote session has none
	// here, so it answers like a read-only attach instead of an error toast.
	if write && action == "reconnect" {
		result := jsoncompat.NewObject()
		result.Set("ok", true)
		result.Set("readOnly", true)
		result.Set("hint", remoteReadOnlyHint)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return true
	}
	if write || action == "goal" {
		h.fail(http.StatusConflict, errorBody(errRemoteReadOnly.Error()))
		return true
	}
	switch action {
	case "":
		h.remoteOutput(h.server.remoteView(host, threadID))
	case "history":
		h.remoteOutput(h.server.remoteStore(host).HistoryFor(threadID, nil))
	case "timeline":
		h.remoteOutput(h.server.withRemoteProjection(host, threadID, func(projection *timeline.Timeline) (*jsoncompat.Object, error) {
			return projection.Page(intQuery(query, "limit", 20), queryValue(query, "before", "")), nil
		}))
	case "detail":
		h.remoteOutput(h.server.withRemoteProjection(host, threadID, func(projection *timeline.Timeline) (*jsoncompat.Object, error) {
			var version any
			if raw := queryValue(query, "version", ""); raw != "" {
				version = raw
			}
			return projection.Detail(queryValue(query, "key", ""), intQuery(query, "offset", 0), version)
		}))
	case "changes":
		h.remoteChanges(host, threadID, query)
	case "poll":
		h.remotePoll(host, threadID, query)
	case "events":
		h.remoteStream(host, threadID, query)
	case "catalog":
		// No remote catalog yet, and the local one must never stand in.
		h.output(http.StatusOK, emptyCatalog(query), contentTypeJSON, "")
	case "permissions":
		h.output(http.StatusOK, unsupportedPermissions(), contentTypeJSON, "")
	case "input":
		h.output(http.StatusOK, noPendingInput(), contentTypeJSON, "")
	default:
		h.fail(http.StatusNotFound, errorBody("接口不存在"))
	}
	return true
}

func (h *handler) remoteOutput(result *jsoncompat.Object, err error) {
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// remoteWaitIdle holds a long-poll that is already up to date: remote pages do
// not refresh live, so without the wait the client would spin.
func (h *handler) remoteWaitIdle() bool {
	select {
	case <-h.request.Context().Done():
		return false
	case <-h.server.Closed():
		return false
	case <-time.After(remoteWait):
		return true
	}
}

func (h *handler) remoteChanges(host remoteHost, threadID string, query map[string][]string) {
	after, epoch := queryValue(query, "after", "-1"), queryValue(query, "epoch", "")
	current, err := h.server.withRemoteProjection(host, threadID, func(projection *timeline.Timeline) (*jsoncompat.Object, error) {
		state := projection.State()
		if state.Epoch == epoch && state.Sequence == after {
			return nil, nil
		}
		return projection.Changes(after, epoch, queryValue(query, "start", "")), nil
	})
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	if current == nil {
		if !h.remoteWaitIdle() {
			return
		}
		current, err = h.server.withRemoteProjection(host, threadID, func(projection *timeline.Timeline) (*jsoncompat.Object, error) {
			return projection.Changes(after, epoch, queryValue(query, "start", "")), nil
		})
	}
	h.remoteOutput(current, err)
}

func (h *handler) remotePoll(host remoteHost, threadID string, query map[string][]string) {
	view, err := h.server.remoteView(host, threadID)
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	result := jsoncompat.NewObject()
	if sequence, _ := view.MustGet("sequence").(json.Number); sequence.String() != queryValue(query, "after", "-1") {
		result.Set("state", view)
	} else {
		if !h.remoteWaitIdle() {
			return
		}
		result.Set("state", nil)
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// remoteStream is the SSE endpoint for a remote session: it brings the
// client's cursor up to date once, then only keeps the connection alive.
func (h *handler) remoteStream(host remoteHost, threadID string, query map[string][]string) {
	since := h.request.Header.Get("Last-Event-ID")
	if since == "" {
		since = queryValue(query, "since", "")
	}
	epoch, after := "", "-1"
	if head, tail, found := strings.Cut(since, "."); found && head != "" && isDigits(tail) {
		epoch, after = head, tail
	}
	var ready *jsoncompat.Object
	cursor := ""
	payload, err := h.server.withRemoteProjection(host, threadID, func(projection *timeline.Timeline) (*jsoncompat.Object, error) {
		state := projection.State()
		sequence, _ := strconv.Atoi(state.Sequence)
		ready = jsoncompat.NewObject()
		ready.Set("epoch", state.Epoch)
		ready.Set("sequence", jsoncompat.Num(sequence))
		cursor = state.Epoch + "." + state.Sequence
		if state.Epoch == epoch && state.Sequence == after {
			return nil, nil
		}
		return projection.Changes(after, epoch, ""), nil
	})
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	h.beginEventStream()
	if h.sseFrame("ready", ready, "") != nil {
		return
	}
	if payload != nil {
		event := "changes"
		if _, reset := payload.Get("reset"); reset {
			event = "reset"
		}
		if h.sseFrame(event, payload, cursor) != nil {
			return
		}
	}
	if queryValue(query, "sync", "") == "1" && h.sseFrame("synchronized", ready, "") != nil {
		return
	}
	for {
		select {
		case <-h.request.Context().Done():
			return
		case <-h.server.Closed():
			return
		case <-time.After(remoteWait):
		}
		if h.authenticatedSession(h.routeSessionKey()) == nil {
			h.sseFrame("logout", &jsoncompat.Object{}, "")
			return
		}
		if h.writeRaw(": heartbeat\n\n") != nil {
			return
		}
	}
}
