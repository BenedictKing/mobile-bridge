package acpruntime

// Permission presets vs work modes. ACP agents publish their permission
// presets on three different surfaces:
//
//   - dsh: the whole modes object (collaboration lives in a config option);
//   - mcode: a config option of category "_permission" (modes is default/plan);
//   - claude, kimi, codebuddy, zcode: the modes object, with plan mixed in.
//
// The phone's work-mode toggle only moves into and out of plan; everything
// else in the modes list is a permission preset the user picks separately,
// so sending a message never resets an agent's own auto/yolo mode.

import (
	"errors"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	planModeID         = "plan"
	permissionCategory = "_permission"
)

type modeLabel struct{ name, description string }

// permissionModeLabels overrides the agents' own wording where it is missing
// or misleading. Kimi's ACP descriptions are swapped relative to its engine:
// "yolo" is the TUI's Ask When Needed (a bash parser still stops dangerous
// commands) and "auto" is Never Ask (everything approved, questions denied).
// Ids absent here keep the agent's name and description.
var permissionModeLabels = map[string]map[string]modeLabel{
	"claude": {
		"default":           {"手动确认", "修改文件或执行命令前都先询问"},
		"acceptEdits":       {"自动接受编辑", "文件编辑自动通过，命令仍需确认"},
		"auto":              {"智能审核（AI）", "Claude 分类器逐个判断：安全的自动执行，有风险的拦下或询问"},
		"bypassPermissions": {"跳过权限确认", "所有操作直接执行，不再询问"},
	},
	"kimi": {
		"default": {"总是询问", "只读操作自动执行，其余操作都需确认"},
		"yolo":    {"按需询问（规则）", "常规编辑和命令自动执行；危险命令、提问和计划仍询问（Ask When Needed）"},
		"auto":    {"从不询问", "全部自动执行，提问也被跳过；仅计划模式的计划等待确认（Never Ask）"},
	},
	"workbuddy": {
		"default":           {"总是询问", "每种工具首次使用时询问"},
		"acceptEdits":       {"自动接受编辑", "文件编辑自动通过，其余仍需确认"},
		"auto":              {"智能审核（AI）", "AI 审查每个需审批的操作；不通过时直接拒绝该操作，不会转为人工确认"},
		"dontAsk":           {"不询问", "只执行预先允许的安全操作，其余直接拒绝"},
		"bypassPermissions": {"跳过权限确认", "跳过所有权限提示"},
		"fullAccess":        {"完全访问", "跳过包括危险命令在内的全部检查"},
	},
	"mcode": {
		"default":           {"询问", "需要审批的操作都先确认"},
		"auto":              {"智能审核（AI）", "分类器自动处理常规操作，必要时询问"},
		"bypassPermissions": {"完全访问", "放开执行权限，不再询问"},
	},
	"zcode": {
		"guarded": {"守护（规则）", "与 Yolo 一样自动执行，但命中审批规则的操作仍询问"},
		"yolo":    {"Yolo", "跳过所有权限询问"},
	},
}

type permissionEntry struct{ id, name, description string }

// permissionSurface is where one session's permission presets live right now.
type permissionSurface struct {
	entries []permissionEntry
	current string
	// option is non-nil when the presets are a config option's values.
	option *jsoncompat.Object
	// inPlan: a modes-surface agent is parked in plan; current is the preset
	// leaving plan will restore.
	inPlan bool
	// modesOnly marks dsh, whose modes object never mixes in work modes.
	modesOnly bool
}

func (surface permissionSurface) has(id string) bool {
	for _, entry := range surface.entries {
		if entry.id == id {
			return true
		}
	}
	return false
}

func (s *Session) permissionSurfaceLocked() permissionSurface {
	if s.kind == "dsh" || s.modeOptionID != "" {
		surface := permissionSurface{modesOnly: true}
		if s.modes != nil {
			surface.entries = modeEntries(s.modes, "")
			surface.current, _ = s.modes.MustGet("currentModeId").(string)
		}
		return surface
	}
	if option := findCategory(s.configOptions, permissionCategory); option != nil {
		surface := permissionSurface{option: option, entries: optionEntries(option)}
		surface.current, _ = option.MustGet("currentValue").(string)
		return surface
	}
	var surface permissionSurface
	if s.modes == nil {
		return surface
	}
	surface.entries = modeEntries(s.modes, planModeID)
	surface.current, _ = s.modes.MustGet("currentModeId").(string)
	if surface.current == planModeID {
		surface.inPlan = true
		surface.current = s.prePlanMode
	}
	return surface
}

func modeEntries(modes *jsoncompat.Object, skip string) []permissionEntry {
	available, _ := modes.MustGet("availableModes").(*jsoncompat.Array)
	if available == nil {
		return nil
	}
	var entries []permissionEntry
	for _, item := range available.Items {
		mode, _ := item.(*jsoncompat.Object)
		if mode == nil {
			continue
		}
		id, _ := mode.MustGet("id").(string)
		if id == "" || id == skip {
			continue
		}
		name, _ := mode.MustGet("name").(string)
		description, _ := mode.MustGet("description").(string)
		entries = append(entries, permissionEntry{id: id, name: name, description: description})
	}
	return entries
}

func optionEntries(option *jsoncompat.Object) []permissionEntry {
	values, _ := option.MustGet("options").(*jsoncompat.Array)
	if values == nil {
		return nil
	}
	var entries []permissionEntry
	for _, item := range values.Items {
		value, _ := item.(*jsoncompat.Object)
		if value == nil {
			continue
		}
		id, _ := value.MustGet("value").(string)
		if id == "" {
			continue
		}
		name, _ := value.MustGet("name").(string)
		description, _ := value.MustGet("description").(string)
		entries = append(entries, permissionEntry{id: id, name: name, description: description})
	}
	return entries
}

// Permissions reports the session's permission presets for the phone's
// picker; a surface with fewer than two presets offers no choice.
func (s *Session) Permissions() *jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	surface := s.permissionSurfaceLocked()
	labels := permissionModeLabels[s.kind]
	options := &jsoncompat.Array{}
	for _, entry := range surface.entries {
		name, description := entry.name, entry.description
		if label, ok := labels[entry.id]; ok {
			name, description = label.name, label.description
		}
		if name == "" {
			name = entry.id
		}
		option := jsoncompat.NewObject()
		option.Set("value", entry.id)
		option.Set("name", name)
		if description != "" {
			option.Set("description", description)
		}
		options.Items = append(options.Items, option)
	}
	result := jsoncompat.NewObject()
	result.Set("supported", len(options.Items) >= 2)
	result.Set("options", options)
	if surface.current != "" {
		result.Set("current", surface.current)
	} else {
		result.Set("current", nil)
	}
	result.Set("planActive", surface.inPlan)
	return result
}

// SetPermission switches the session's permission preset. While a
// modes-surface agent sits in plan the choice is only remembered and applied
// when plan ends, so picking a preset never silently drops the plan.
func (s *Session) SetPermission(modeID string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.readOnly {
		s.mu.Unlock()
		return errors.New(ReadOnlySessionMessage)
	}
	surface := s.permissionSurfaceLocked()
	if len(surface.entries) < 2 {
		s.mu.Unlock()
		return errors.New("当前 Agent 不支持移动端权限模式设置")
	}
	if !surface.has(modeID) {
		s.mu.Unlock()
		return errors.New("当前 Agent 不支持此权限模式")
	}
	if surface.current == modeID {
		s.mu.Unlock()
		return nil
	}
	if surface.inPlan {
		s.prePlanMode = modeID
		s.signalLocked()
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if surface.option != nil {
		_, err := s.setConfigOption(surface.option, modeID)
		return err
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", s.ID)
	params.Set("modeId", modeID)
	if _, err := s.Process.Request("session/set_mode", params, 30*time.Second); err != nil {
		return err
	}
	s.mu.Lock()
	s.modes.Set("currentModeId", modeID)
	if !surface.modesOnly {
		// The modes slot doubles as the view's collaboration mode here.
		s.View.SetMode(modeID)
	}
	s.signalLocked()
	s.mu.Unlock()
	return nil
}

// ApplyWorkMode moves the session into or out of plan for one phone send.
// modeMap translates the bridge's plan/default ids (zcode default→yolo,
// opencode default→build). On modes-surface agents "default" means "not
// plan": outside plan it is a no-op, so the user's permission preset (Claude
// auto, Kimi yolo, ZCode guarded…) survives every message; leaving plan
// restores the preset that was active before it.
func (s *Session) ApplyWorkMode(workMode string, modeMap map[string]string) error {
	target := workMode
	if mapped, ok := modeMap[workMode]; ok {
		target = mapped
	}
	planID := planModeID
	if mapped, ok := modeMap["plan"]; ok {
		planID = mapped
	}
	s.mu.Lock()
	if s.modeOptionID != "" || s.modes == nil {
		// Collaboration lives in a dedicated option (dsh) or a plan/build
		// option (opencode): the toggle is the whole surface.
		s.mu.Unlock()
		return s.SetMode(target)
	}
	current, _ := s.modes.MustGet("currentModeId").(string)
	if workMode == "plan" {
		if current != "" && current != planID {
			s.prePlanMode = current
		}
		s.mu.Unlock()
		return s.SetMode(target)
	}
	if current != "" && current != planID {
		s.mu.Unlock()
		return nil
	}
	restore := s.prePlanMode != ""
	if restore {
		target = s.prePlanMode
	}
	s.mu.Unlock()
	if err := s.SetMode(target); err != nil {
		return err
	}
	if restore {
		s.mu.Lock()
		s.prePlanMode = ""
		s.mu.Unlock()
	}
	return nil
}

// notePlanEntryLocked remembers the preset an agent leaves when it enters
// plan on its own (Claude's EnterPlanMode tool), so a later "default" send
// restores it instead of the manual default.
func (s *Session) notePlanEntryLocked(previous, next string) {
	if s.kind == "dsh" || s.modeOptionID != "" {
		return
	}
	if next == planModeID && previous != "" && previous != planModeID {
		s.prePlanMode = previous
	}
}
