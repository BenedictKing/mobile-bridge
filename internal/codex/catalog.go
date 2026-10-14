package codex

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Catalog reads the model/Skill directory through a short-lived Codex
// app-server connection (bridge/catalog.py port). Models and Skills cache
// in memory for five minutes per cwd, like Python's kind caches; the API
// branch (custom providers) discovers model ids from the provider's own
// /models endpoint without following redirects.

const catalogTTL = 5 * time.Minute

var codexEffortFallback = []string{"low", "medium", "high", "xhigh", "ultra"}

var catalogModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:@+-]{0,199}$`)

type skillRow struct {
	ID          string
	Name        string
	Description string
	Path        string
	Scope       any
	DisplayName string
}

type modelsCacheEntry struct {
	at    time.Time
	value *jsoncompat.Object
}

type skillsCacheEntry struct {
	at     time.Time
	rows   []skillRow
	errors []string
}

type Catalog struct {
	bridge *Bridge

	mu     sync.Mutex
	models map[string]modelsCacheEntry
	skills map[string]skillsCacheEntry
}

// catalogOnce lives on the Bridge so every handler shares one cache.
func (b *Bridge) Catalog() *Catalog {
	b.catalogOnce.Do(func() {
		b.catalog = &Catalog{bridge: b, models: map[string]modelsCacheEntry{},
			skills: map[string]skillsCacheEntry{}}
	})
	return b.catalog
}

func (c *Catalog) executable() (string, error) {
	if c.bridge.Home == "" {
		return "", errors.New("找不到 Codex 数据目录")
	}
	name := c.bridge.Executable
	if name == "" {
		name = os.Getenv("GATEWAY_CODEX_BIN")
	}
	if name == "" {
		name, _ = exec.LookPath("codex")
	}
	if name == "" {
		return "", errors.New("找不到桌面 App 的 Codex 运行时")
	}
	return name, nil
}

// fetch opens one short-lived app-server connection and hands the typed
// request closure the RPC surface (initialize plus read-only methods).
func (c *Catalog) fetch(cwd string, run func(request func(method string, params any) (*jsoncompat.Object, error)) error) error {
	executable, err := c.executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "app-server", "--listen", "stdio://")
	command.Dir = cwd
	command.Env = append(os.Environ(), "CODEX_HOME="+c.bridge.Home)
	stdin, err := command.StdinPipe()
	if err != nil {
		return errors.New("无法连接桌面目录服务")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return errors.New("无法连接桌面目录服务")
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return errors.New("无法启动桌面目录服务")
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	request := func(method string, params any) (*jsoncompat.Object, error) {
		return appRequestTimeout(stdin, scanner, method, params, 90*time.Second)
	}
	if _, err := request("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "codex_mobile_catalog", "title": "Mobile catalog", "version": "0.2"},
		"capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return err
	}
	if _, err := stdin.Write([]byte("{\"method\":\"initialized\"}\n")); err != nil {
		return err
	}
	return run(request)
}

// Models returns the codex model directory for one working directory:
// {kind:"models", models[], modelSource, fastMode, currentEffort, modelError?}.
// currentModel stays empty for the caller to fold in from the live thread.
func (c *Catalog) Models(cwd, provider string, refresh bool) (*jsoncompat.Object, error) {
	key := cwd + "\x00" + provider
	c.mu.Lock()
	if cached, ok := c.models[key]; ok && !refresh && time.Since(cached.at) < catalogTTL {
		value := cached.value
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()
	raw := jsoncompat.NewObject()
	raw.Set("models", &jsoncompat.Array{})
	raw.Set("modelSource", "codex")
	var fastMode map[string]any
	err := c.fetch(cwd, func(request func(method string, params any) (*jsoncompat.Object, error)) error {
		value, fetchErr := readModels(request, cwd, provider, c.bridge.Home)
		if value != nil {
			raw = value
		}
		if fetchErr != nil {
			return fetchErr
		}
		if source, _ := raw.MustGet("modelSource").(string); source != "api" {
			fastMode, _ = fastModeOf(request, cwd)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if fastMode == nil {
		fastMode = map[string]any{"allowed": false}
	}
	source, _ := raw.MustGet("modelSource").(string)
	models := &jsoncompat.Array{}
	if rows, ok := raw.MustGet("models").(*jsoncompat.Array); ok {
		for _, item := range rows.Items {
			model, ok := item.(*jsoncompat.Object)
			if !ok {
				continue
			}
			if hidden, ok := model.MustGet("hidden").(bool); ok && hidden {
				continue
			}
			entry := jsoncompat.NewObject()
			identifier := stringOr(model, "model", stringOr(model, "id", ""))
			entry.Set("id", identifier)
			entry.Set("name", stringOr(model, "displayName", stringOr(model, "model", "")))
			entry.Set("description", stringOr(model, "description", ""))
			efforts := &jsoncompat.Array{}
			if list, ok := model.MustGet("supportedReasoningEfforts").(*jsoncompat.Array); ok {
				for _, effortItem := range list.Items {
					if effort := objOf(effortItem); effort != nil {
						if value, ok := effort.MustGet("reasoningEffort").(string); ok && value != "" {
							efforts.Items = append(efforts.Items, value)
						}
					}
				}
			}
			if len(efforts.Items) == 0 {
				for _, fallback := range codexEffortFallback {
					efforts.Items = append(efforts.Items, fallback)
				}
			}
			entry.Set("efforts", efforts)
			entry.Set("defaultEffort", stringOr(model, "defaultReasoningEffort", ""))
			entry.Set("fastTier", fastTierOf(model))
			if tier := stringOr(model, "defaultServiceTier", ""); tier != "" {
				entry.Set("defaultServiceTier", tier)
			}
			models.Items = append(models.Items, entry)
		}
	}
	value := jsoncompat.NewObject()
	value.Set("kind", "models")
	value.Set("models", models)
	value.Set("modelSource", source)
	fastObject := jsoncompat.NewObject()
	for _, key := range sortedKeys(fastMode) {
		fastObject.Set(key, fastMode[key])
	}
	value.Set("fastMode", fastObject)
	if modelError, ok := raw.Get("modelError"); ok {
		value.Set("modelError", modelError)
	}
	value.Set("currentEffort", raw.MustGet("currentEffort"))
	c.mu.Lock()
	c.models[key] = modelsCacheEntry{at: time.Now(), value: value}
	c.mu.Unlock()
	return value, nil
}

// Skills lists the cwd's Skill rows through the same short-lived runtime.
func (c *Catalog) Skills(cwd string, refresh bool) ([]skillRow, []string, error) {
	c.mu.Lock()
	if cached, ok := c.skills[cwd]; ok && !refresh && time.Since(cached.at) < catalogTTL {
		rows, errs := cached.rows, cached.errors
		c.mu.Unlock()
		return rows, errs, nil
	}
	c.mu.Unlock()
	var rows []skillRow
	var errs []string
	err := c.fetch(cwd, func(request func(method string, params any) (*jsoncompat.Object, error)) error {
		result, fetchErr := request("skills/list", map[string]any{"cwds": []string{cwd}, "forceReload": false})
		if fetchErr != nil {
			return fetchErr
		}
		rows, errs = skillRowsOf(result, cwd)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	c.skills[cwd] = skillsCacheEntry{at: time.Now(), rows: rows, errors: errs}
	c.mu.Unlock()
	return rows, errs, nil
}

// ValidateSkills resolves picker ids back to {id,name,path} rows, mirroring
// catalog_reader.validate_skills (missing ids refuse the send).
func (c *Catalog) ValidateSkills(cwd string, ids []string, refresh bool) ([]map[string]string, error) {
	deduped := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] && len(deduped) < 8 {
			seen[id] = true
			deduped = append(deduped, id)
		}
	}
	rows, _, err := c.Skills(cwd, refresh)
	if err != nil {
		return nil, errors.New("Skill 不可用，请刷新列表")
	}
	byID := map[string]skillRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	selected := []map[string]string{}
	for _, id := range deduped {
		row, ok := byID[id]
		if !ok && !refresh {
			// A stale cache may simply predate the skill: retry once fresh.
			return c.ValidateSkills(cwd, ids, true)
		}
		if !ok {
			return nil, errors.New("Skill 不可用，请刷新列表")
		}
		selected = append(selected, map[string]string{"id": row.ID, "name": row.Name, "path": row.Path})
	}
	return selected, nil
}

func readModels(request func(method string, params any) (*jsoncompat.Object, error), cwd, provider, home string) (*jsoncompat.Object, error) {
	configResult, err := request("config/read", map[string]any{"includeLayers": false, "cwd": cwd})
	if err != nil {
		return nil, err
	}
	config, _ := configResult.MustGet("config").(*jsoncompat.Object)
	if config == nil {
		config = jsoncompat.NewObject()
	}
	effective := effectiveConfig(config)
	if provider == "" {
		provider, _ = effective.MustGet("model_provider").(string)
		if provider == "" {
			provider = "openai"
		}
	}
	definition, _ := effective.MustGet("model_providers").(*jsoncompat.Object)
	providerDefinition := jsoncompat.NewObject()
	if definition != nil {
		if raw, ok := definition.MustGet(provider).(*jsoncompat.Object); ok {
			providerDefinition = raw
		}
	}
	base := stringOf(providerDefinition, "base_url", "")
	if base == "" && provider == "openai" {
		base, _ = effective.MustGet("openai_base_url").(string)
		if base == "" {
			base = os.Getenv("OPENAI_BASE_URL")
		}
	}
	custom := provider != "openai" || base != "" || anyOf(providerDefinition,
		"env_key", "experimental_bearer_token", "http_headers", "env_http_headers", "auth", "gateway_oauth")
	result := jsoncompat.NewObject()
	result.Set("currentEffort", effective.MustGet("model_reasoning_effort"))
	if !custom {
		if authResult, err := request("account/read", map[string]any{"refreshToken": false}); err == nil {
			if account := objOf(authResult.MustGet("account")); account != nil {
				if kind, _ := account.MustGet("type").(string); kind == "apiKey" {
					custom = true
				}
			}
		}
	}
	if !custom {
		models := &jsoncompat.Array{}
		cursor := any(nil)
		for page := 0; page < 10; page++ {
			params := map[string]any{"cursor": cursor, "limit": 100, "includeHidden": false}
			response, err := request("model/list", params)
			if err != nil {
				return nil, err
			}
			if list, ok := response.MustGet("data").(*jsoncompat.Array); ok {
				models.Items = append(models.Items, list.Items...)
			}
			next, _ := response.MustGet("nextCursor").(string)
			if next == "" {
				break
			}
			cursor = next
		}
		result.Set("models", models)
		result.Set("modelSource", "codex")
		return result, nil
	}
	// API-key providers: discover ids from the provider endpoint. Credential
	// and endpoint problems become modelError, never a failed request.
	result.Set("models", &jsoncompat.Array{})
	result.Set("modelSource", "api")
	ids, modelErr := apiModelIDs(provider, providerDefinition, base, home)
	if modelErr == nil {
		models := &jsoncompat.Array{}
		for _, id := range ids {
			entry := jsoncompat.NewObject()
			entry.Set("model", id)
			entry.Set("displayName", id)
			models.Items = append(models.Items, entry)
		}
		result.Set("models", models)
	} else {
		result.Set("modelError", modelErr.Error())
	}
	return result, nil
}

func apiModelIDs(provider string, definition *jsoncompat.Object, base, home string) ([]string, error) {
	if anyOf(definition, "http_headers", "env_http_headers", "auth", "gateway_oauth", "query_params", "aws") {
		return nil, errors.New("此 API 接入使用额外认证配置，请手动填写模型 ID")
	}
	if base == "" && provider != "openai" {
		return nil, errors.New("未找到当前 API 地址，请检查接入配置")
	}
	key := os.Getenv(stringOf(definition, "env_key", ""))
	if key == "" {
		key = stringOf(definition, "experimental_bearer_token", "")
	}
	envKey := stringOf(definition, "env_key", "")
	if envKey != "" && key == "" {
		return nil, errors.New("未找到当前 API Key，请检查接入配置")
	}
	if key == "" && (provider == "openai" || truthy(definition.MustGet("requires_openai_auth"))) {
		key = savedOpenAIKey(home)
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" {
			return nil, errors.New("未找到当前 API Key，请检查接入配置")
		}
	}
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Scheme != "https" && endpoint.Scheme != "http" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("未找到当前 API 地址，请检查接入配置")
	}
	client := &http.Client{
		Timeout: 12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	target := strings.TrimRight(base, "/") + "/models"
	httpRequest, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("未找到当前 API 地址，请检查接入配置")
	}
	if key != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+key)
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", "Mobile-Bridge")
	response, err := client.Do(httpRequest)
	if err != nil {
		return nil, errors.New("无法连接上游，请检查 API 地址、网络和证书后重试")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, errors.New("上游拒绝访问，请检查 API Key 和模型列表权限")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, errors.New("上游要求跳转，请填写最终 API 地址后重试")
	}
	if response.StatusCode >= 300 {
		return nil, errors.New("上游暂不提供模型列表，请检查 API 地址或手动填写模型 ID")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil {
		return nil, errors.New("无法连接上游，请检查 API 地址、网络和证书后重试")
	}
	if len(body) > 1024*1024 {
		return nil, errors.New("上游模型列表过大，请手动填写模型 ID")
	}
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return nil, errors.New("上游未返回兼容的模型列表，请手动填写模型 ID")
	}
	rows := any(nil)
	if object, ok := decoded.(map[string]any); ok {
		rows = object["data"]
	} else {
		rows = decoded
	}
	list, ok := rows.([]any)
	if !ok {
		return nil, errors.New("上游未返回兼容的模型列表，请手动填写模型 ID")
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, ok := row["id"].(string)
		if !ok || !catalogModelID.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("上游未返回可用模型，请手动填写模型 ID")
	}
	sort.Strings(ids)
	return ids, nil
}

func fastModeOf(request func(method string, params any) (*jsoncompat.Object, error), cwd string) (map[string]any, error) {
	unavailable := map[string]any{"allowed": false}
	configResult, err := request("config/read", map[string]any{"includeLayers": false, "cwd": cwd})
	if err != nil {
		return unavailable, nil
	}
	config, _ := configResult.MustGet("config").(*jsoncompat.Object)
	if config == nil {
		return unavailable, nil
	}
	effective := effectiveConfig(config)
	provider, _ := effective.MustGet("model_provider").(string)
	if provider == "" {
		provider = "openai"
	}
	definitions, _ := effective.MustGet("model_providers").(*jsoncompat.Object)
	if provider != "openai" || stringOf(effective, "openai_base_url", "") != "" {
		return unavailable, nil
	}
	if definitions != nil {
		if raw, ok := definitions.MustGet(provider).(*jsoncompat.Object); ok && anyOf(raw,
			"base_url", "env_key", "experimental_bearer_token", "http_headers", "env_http_headers", "auth", "gateway_oauth") {
			return unavailable, nil
		}
	}
	authResult, err := request("account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return unavailable, nil
	}
	account := objOf(authResult.MustGet("account"))
	kind, _ := account.MustGet("type").(string)
	requires, ok := authResult.MustGet("requiresOpenaiAuth").(bool)
	if kind != "chatgpt" || !ok || !requires {
		return unavailable, nil
	}
	requirementsResult, err := request("configRequirements/read", map[string]any{})
	if err != nil {
		return unavailable, nil
	}
	requirements, _ := requirementsResult.MustGet("requirements").(*jsoncompat.Object)
	allowed := true
	if features := objOf(effective.MustGet("features")); features != nil {
		if value, ok := features.MustGet("fast_mode").(bool); ok && !value {
			allowed = false
		}
	}
	if requirements != nil {
		if featureRequirements := objOf(requirements.MustGet("featureRequirements")); featureRequirements != nil {
			if value, ok := featureRequirements.MustGet("fast_mode").(bool); ok && !value {
				allowed = false
			}
		}
	}
	result := map[string]any{"allowed": allowed}
	if tier := stringOf(effective, "service_tier", ""); tier != "" {
		result["defaultServiceTier"] = tier
	}
	return result, nil
}

// effectiveConfig folds the active profile over the base config.
func effectiveConfig(config *jsoncompat.Object) *jsoncompat.Object {
	effective := jsoncompat.NewObject()
	for _, key := range config.Keys() {
		value, _ := config.Get(key)
		effective.Set(key, value)
	}
	profileName, _ := config.MustGet("profile").(string)
	if profiles := objOf(config.MustGet("profiles")); profiles != nil && profileName != "" {
		if profile := objOf(profiles.MustGet(profileName)); profile != nil {
			for _, key := range profile.Keys() {
				value, _ := profile.Get(key)
				effective.Set(key, value)
			}
		}
	}
	return effective
}

func skillRowsOf(result *jsoncompat.Object, cwd string) ([]skillRow, []string) {
	rows := []skillRow{}
	errs := []string{}
	entries, _ := result.MustGet("data").(*jsoncompat.Array)
	if entries == nil {
		if single := objOf(result.MustGet("skillEntries")); single != nil {
			entries = &jsoncompat.Array{Items: []any{single}}
		}
	}
	if entries == nil {
		return rows, errs
	}
	for _, item := range entries.Items {
		entry := objOf(item)
		if entry == nil {
			continue
		}
		if problems, ok := entry.MustGet("errors").(*jsoncompat.Array); ok {
			for _, problem := range problems.Items {
				if text, ok := problem.(string); ok {
					errs = append(errs, text)
				}
			}
		}
		skills, _ := entry.MustGet("skills").(*jsoncompat.Array)
		if skills == nil {
			continue
		}
		for _, raw := range skills.Items {
			skill := objOf(raw)
			if skill == nil {
				continue
			}
			if disabled, ok := skill.MustGet("enabled").(bool); ok && !disabled {
				continue
			}
			path, _ := skill.MustGet("path").(string)
			if path == "" {
				continue
			}
			if !strings.HasPrefix(path, "/") && !strings.Contains(path, "\\") {
				path = cwd + "/" + path
			}
			name, _ := skill.MustGet("name").(string)
			display := name
			if face := objOf(skill.MustGet("interface")); face != nil {
				if text, ok := face.MustGet("displayName").(string); ok && text != "" {
					display = text
				}
			}
			digest := sha256.Sum256([]byte(path))
			rows = append(rows, skillRow{
				ID:          hex.EncodeToString(digest[:]),
				Name:        name,
				Description: stringOr(skill, "description", ""),
				Path:        path,
				Scope:       skill.MustGet("scope"),
				DisplayName: display,
			})
		}
	}
	return rows, errs
}

func fastTierOf(model *jsoncompat.Object) string {
	tiers, _ := model.MustGet("serviceTiers").(*jsoncompat.Array)
	if tiers == nil {
		return ""
	}
	for _, item := range tiers.Items {
		tier := objOf(item)
		if tier == nil {
			continue
		}
		id, _ := tier.MustGet("id").(string)
		name, _ := tier.MustGet("name").(string)
		if id == "fast" || id == "priority" ||
			strings.TrimSpace(strings.ToLower(name)) == "fast" || strings.TrimSpace(strings.ToLower(name)) == "priority" {
			return id
		}
	}
	return ""
}

func stringOr(object *jsoncompat.Object, key, fallback string) string {
	if object == nil {
		return fallback
	}
	if value, ok := object.MustGet(key).(string); ok && value != "" {
		return value
	}
	return fallback
}

func stringOf(object *jsoncompat.Object, key, fallback string) string {
	if object == nil {
		return fallback
	}
	if value, ok := object.MustGet(key).(string); ok {
		return value
	}
	return fallback
}

func anyOf(object *jsoncompat.Object, keys ...string) bool {
	for _, key := range keys {
		if value, exists := object.Get(key); exists && jsoncompat.Truthy(value) {
			return true
		}
	}
	return false
}

func truthy(value any) bool { return jsoncompat.Truthy(value) }

func objOf(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

func sortedKeys[V any](mapping map[string]V) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func savedOpenAIKey(home string) string {
	if home == "" {
		return ""
	}
	raw, err := os.ReadFile(home + "/auth.json")
	if err != nil {
		return ""
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return ""
	}
	key, _ := decoded["OPENAI_API_KEY"].(string)
	return key
}

// appRequestTimeout is appRequest with a configurable deadline and the
// caller's own error message surface (the catalog surfaces runtime errors
// like "目录读取失败" instead of the create flow's wording).
func appRequestTimeout(stdin io.Writer, scanner *bufio.Scanner, method string, params any, timeout time.Duration) (*jsoncompat.Object, error) {
	requestID := appRequestID.Add(1)
	payload := map[string]any{"id": requestID, "method": method, "params": params}
	data, _ := json.Marshal(payload)
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return nil, errors.New("桌面目录查询已断开")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && scanner.Scan() {
		var response map[string]any
		if json.Unmarshal(scanner.Bytes(), &response) != nil {
			continue
		}
		if jsoncompat.PyStr(response["id"]) != strconv.FormatUint(requestID, 10) {
			continue
		}
		if response["error"] != nil {
			message := "目录读取失败"
			if problem, ok := response["error"].(map[string]any); ok {
				if text, ok := problem["message"].(string); ok && text != "" {
					message = text
				}
			}
			return nil, errors.New(message)
		}
		raw, _ := json.Marshal(response["result"])
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			return nil, errors.New("目录读取失败")
		}
		object, _ := value.(*jsoncompat.Object)
		if object == nil {
			return jsoncompat.NewObject(), nil
		}
		return object, nil
	}
	return nil, errors.New("桌面模型/Skill 目录读取超时")
}
