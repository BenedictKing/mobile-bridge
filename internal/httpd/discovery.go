package httpd

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// listDiscovery 定时重读各 Agent 的本机会话列表（Python bridge/discovery.py
// 的 Go 对应）：身份、标题或项目的变化折叠成单调递增的 revision，
// /api/activity 随行下发，网页的 activity 轮询发现版本变化即自动重载列表，
// 在终端或桌面 App 中新开的会话约一个发现周期内自行出现。没有客户端轮询
// activity 超过 idleAfter 时跳过本轮，不为无人观看的列表 spawn Agent 进程。
type listDiscovery struct {
	server    *Server
	interval  time.Duration
	idleAfter time.Duration

	mu          sync.Mutex
	revision    int
	fingerprint string
	seen        bool
	watchedAt   time.Time

	stop    chan struct{}
	stopped sync.Once
}

func newListDiscovery(server *Server) *listDiscovery {
	return &listDiscovery{
		server:    server,
		interval:  10 * time.Second,
		idleAfter: 120 * time.Second,
		watchedAt: time.Now(),
		stop:      make(chan struct{}),
	}
}

// touch 记录客户端仍在观看会话列表。
func (d *listDiscovery) touch() {
	d.mu.Lock()
	d.watchedAt = time.Now()
	d.mu.Unlock()
}

// Revision 返回当前列表版本。
func (d *listDiscovery) Revision() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.revision
}

func (d *listDiscovery) run() {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
		}
		d.mu.Lock()
		watched := time.Since(d.watchedAt) <= d.idleAfter
		d.mu.Unlock()
		if !watched {
			continue
		}
		d.refresh()
	}
}

func (d *listDiscovery) close() {
	d.stopped.Do(func() { close(d.stop) })
}

// refresh 重读各来源的会话列表，身份/标题/项目变化时 revision 自增。
// recency 的变化由 activity 轮询单独下发，不触发列表重载。单来源失败只
// 跳过该来源，不影响其他来源与下一轮。
func (d *listDiscovery) refresh() {
	entries := []string{}
	if sessionStore := d.server.Store; sessionStore != nil {
		if rows, err := sessionStore.List("", 100000, 0, false); err == nil {
			for _, row := range rows {
				id, _ := row.MustGet("id").(string)
				title, _ := row.MustGet("title").(string)
				if title == "" {
					title, _ = row.MustGet("name").(string)
				}
				cwd, _ := row.MustGet("cwd").(string)
				entries = append(entries, "codex\x00"+id+"\x00"+title+"\x00"+cwd)
			}
		}
	}
	for kind, manager := range d.server.acpAgentSet() {
		if manager == nil {
			continue
		}
		// 强制刷新 session/list 缓存，本轮发现读取最新快照。
		manager.RefreshList()
		rows, err := manager.List(15 * time.Second)
		if err != nil {
			continue
		}
		for _, row := range rows {
			id, _ := row.MustGet("sessionId").(string)
			if id == "" {
				continue
			}
			title, _ := row.MustGet("title").(string)
			cwd, _ := row.MustGet("cwd").(string)
			entries = append(entries, kind+"\x00"+id+"\x00"+title+"\x00"+cwd)
		}
	}
	sort.Strings(entries)
	fingerprint := strings.Join(entries, "\n")
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.seen || fingerprint != d.fingerprint {
		d.seen = true
		d.fingerprint = fingerprint
		d.revision++
	}
}
