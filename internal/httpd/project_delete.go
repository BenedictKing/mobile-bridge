package httpd

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// hiddenProjects is a durable list of working directories hidden from project
// pickers and chat lists. It never deletes project or session data.
type hiddenProjects struct {
	mu    sync.Mutex
	path  string
	cwds  map[string]bool
	order []string
}

func LoadHiddenProjects(path string) *hiddenProjects {
	ledger := &hiddenProjects{path: path, cwds: map[string]bool{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ledger
	}
	var decoded []string
	if json.Unmarshal(raw, &decoded) == nil {
		for _, cwd := range decoded {
			cwd = filepath.Clean(cwd)
			if cwd == "" || cwd == "." || ledger.cwds[cwd] {
				continue
			}
			ledger.cwds[cwd] = true
			ledger.order = append(ledger.order, cwd)
		}
	}
	return ledger
}

func (h *hiddenProjects) hidden(cwd string) bool {
	if h == nil || cwd == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cwds[filepath.Clean(cwd)]
}

func (h *hiddenProjects) hiddenKeyCwd(key string) string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cwd := range h.order {
		if key == "codex:"+hashProject(cwd) || key == "acp:"+hashProject(cwd) {
			return cwd
		}
	}
	return ""
}

func (h *hiddenProjects) hide(cwd string) (string, error) {
	if h == nil {
		return "", errors.New("当前网关未启用项目隐藏")
	}
	cwd = filepath.Clean(cwd)
	if cwd == "" || cwd == "." {
		return "", errors.New("项目目录无效")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cwds[cwd] {
		return cwd, nil
	}
	h.cwds[cwd] = true
	h.order = append(h.order, cwd)
	if err := h.saveLocked(); err != nil {
		delete(h.cwds, cwd)
		h.order = h.order[:len(h.order)-1]
		return "", err
	}
	return cwd, nil
}

func (h *hiddenProjects) saveLocked() error {
	if h.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(h.order, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

func (h *hiddenProjects) list() *jsoncompat.Array {
	rows := &jsoncompat.Array{}
	if h == nil {
		return rows
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cwd := range h.order {
		rows.Items = append(rows.Items, cwd)
	}
	return rows
}

func (s *Server) hiddenProject(cwd string) bool {
	return s.HiddenProjects.hidden(cwd)
}

// handleProjectDelete serves DELETE /api/projects/:key. The key is resolved
// before hiding, so both codex and acp aliases of the directory disappear.
func (h *handler) handleProjectDelete(path string) {
	key := strings.TrimPrefix(path, "/api/projects/")
	if key == "" || strings.Contains(key, "/") {
		h.fail(http.StatusNotFound, errorBody("项目不存在，请刷新列表"))
		return
	}
	if h.server.HiddenProjects == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用项目隐藏"))
		return
	}
	cwd := h.server.HiddenProjects.hiddenKeyCwd(key)
	if cwd == "" {
		cwd = h.resolveProjectCwd(key)
	}
	if cwd == "" {
		h.fail(http.StatusNotFound, errorBody("项目不存在，请刷新列表"))
		return
	}
	hidden, err := h.server.HiddenProjects.hide(cwd)
	if err != nil {
		h.fail(http.StatusInternalServerError, errorBody("项目隐藏记录保存失败: "+err.Error()))
		return
	}
	result := jsoncompat.NewObject()
	result.Set("key", key)
	result.Set("cwd", hidden)
	result.Set("status", "hidden")
	h.output(http.StatusOK, result, contentTypeJSON, "")
}
