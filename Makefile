# Go 网关（docs/go-migration-plan.md 已收官；Python 实现已退役删除）。
# 日常入口：make start / make stop / make restart / make status / make logs。

PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

LAUNCHD_LABEL := com.mobile-bridge.gateway
LAUNCHD_PLIST := $(HOME)/Library/LaunchAgents/$(LAUNCHD_LABEL).plist
LAUNCHD_LOG := $(HOME)/Library/Logs/mobile-bridge-gateway.log
GATEWAY_DATA_DIR ?= $(CURDIR)/.local
GATEWAY_PORT ?= 8787
INSTALL_BIN := $(HOME)/.local/bin/mobile-bridge-gateway
ACP_HOST_KIND ?= kimi
ACP_HOST_BIN ?= $(HOME)/.kimi-code/bin/kimi
ACP_HOST_ARG ?= acp
ACP_HOST_SOCKET ?= $(GATEWAY_DATA_DIR)/$(ACP_HOST_KIND).sock
ACP_HOST_LABEL := com.mobile-bridge.agent-host.$(ACP_HOST_KIND)
ACP_HOST_PLIST := $(HOME)/Library/LaunchAgents/$(ACP_HOST_LABEL).plist
ACP_HOST_LOG := $(HOME)/Library/Logs/mobile-bridge-agent-host-$(ACP_HOST_KIND).log
ACP_HOST_INSTALL_BIN := $(HOME)/.local/bin/mobile-bridge-agent-host
ACP_HOST_SOCKETS_JSON := {"$(ACP_HOST_KIND)":"$(ACP_HOST_SOCKET)"}

.PHONY: build-matrix build-gateway-package build-agent-host build-wails serve \
	start start-agent-host stop stop-agent-host restart status logs uninstall conformance

build-matrix:
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags='-s -w' \
			-o dist/gateway-$$os-$$arch ./cmd/gateway || exit 1; \
	done
	@ls -lh dist/ | grep gateway-

build-gateway:
	go build -o dist/gateway-go ./cmd/gateway

# The Wails desktop shell expects this stable runtime directory/name. The Go
# binary serves the gateway and exposes the private desktop action protocol.
build-gateway-package:
	@mkdir -p dist/gateway
	@go build -trimpath -ldflags='-s -w' -o dist/gateway/codex-mobile-gateway ./cmd/gateway
	@chmod 755 dist/gateway/codex-mobile-gateway
	@ls -lh dist/gateway/codex-mobile-gateway

build-agent-host:
	@mkdir -p dist/gateway
	@go build -trimpath -ldflags='-s -w' -o dist/gateway/mobile-bridge-agent-host ./cmd/agent-host
	@chmod 755 dist/gateway/mobile-bridge-agent-host
	@ls -lh dist/gateway/mobile-bridge-agent-host

start-agent-host: build-agent-host
	@mkdir -p $(dir $(ACP_HOST_INSTALL_BIN)) $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	@cp dist/gateway/mobile-bridge-agent-host $(ACP_HOST_INSTALL_BIN)
	@chmod 755 $(ACP_HOST_INSTALL_BIN)
	@sed -e "s|@HOME@|$(HOME)|g" -e "s|@ACP_HOST_LABEL@|$(ACP_HOST_LABEL)|g" \
		-e "s|@ACP_HOST_SOCKET@|$(ACP_HOST_SOCKET)|g" -e "s|@ACP_HOST_BIN@|$(ACP_HOST_BIN)|g" \
		-e "s|@ACP_HOST_ARG@|$(ACP_HOST_ARG)|g" -e "s|@ACP_HOST_LOG@|$(ACP_HOST_LOG)|g" \
		deploy/launchd/agent-host.plist.template > $(ACP_HOST_PLIST)
	@launchctl bootout gui/$$(id -u)/$(ACP_HOST_LABEL) 2>/dev/null || true
	@launchctl bootstrap gui/$$(id -u) $(ACP_HOST_PLIST)
	@launchctl kickstart gui/$$(id -u)/$(ACP_HOST_LABEL)
	@sleep 1
	@echo "$(ACP_HOST_LABEL) 已启动（socket $(ACP_HOST_SOCKET)）"

# Wails is the Go desktop shell. It embeds wails/frontend and calls the Go
# gateway action protocol directly.
build-wails:
	@mkdir -p dist/desktop
	@cd wails && go build -trimpath -ldflags='-s -w' -o ../dist/desktop/mobile-bridge-wails .
	@ls -lh dist/desktop/mobile-bridge-wails

# 前台运行（调试用；生产请用 make start 走 launchd 常驻）。
# GATEWAY_DATA_DIR / GATEWAY_PORT 可覆盖以隔离验证。
serve: build-gateway-package
	@GATEWAY_PORT=$(GATEWAY_PORT) ./dist/gateway/codex-mobile-gateway serve --data-dir "$(GATEWAY_DATA_DIR)"

# launchd 常驻：写入 plist、加载并启动（KeepAlive 自动拉活）。
# 幂等——已加载时先卸载再以当前二进制重启。
start: build-gateway-package start-agent-host
	@mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	@sed -e "s|@CURDIR@|$(CURDIR)|g" -e "s|@HOME@|$(HOME)|g" \
		deploy/launchd/gateway.plist.template > $(LAUNCHD_PLIST)
	@launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true
	@launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST)
	@launchctl kickstart gui/$$(id -u)/$(LAUNCHD_LABEL)
	@sleep 1
	@pid=$$(launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | awk -F' = ' '/^\tpid = /{print $$2; exit}'); \
		echo "$(LAUNCHD_LABEL) 已启动（pid $${pid:-?}，KeepAlive 常驻，崩溃自动拉活）"; \
		echo "日志: make logs   状态: make status"

# 彻底停止：bootout 卸载服务，KeepAlive 不会复活；下次 make start 再拉起。
stop-agent-host:
	@launchctl bootout gui/$$(id -u)/$(ACP_HOST_LABEL) 2>/dev/null || true
	@pkill -f "mobile-bridge-agent-host --socket" 2>/dev/null || true

stop: stop-agent-host
	@launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null && echo "$(LAUNCHD_LABEL) 已停止并卸载" || echo "$(LAUNCHD_LABEL) 未在运行"
	@pkill -f "codex-mobile-gateway serve" 2>/dev/null || true

restart: stop start

status:
	@echo "== mobile-bridge 网关 =="
	@if launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) >/dev/null 2>&1; then \
		state=$$(launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | awk -F' = ' '/^\tstate = /{print $$2; exit}'); \
		pid=$$(launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | awk -F' = ' '/^\tpid = /{print $$2; exit}'); \
		echo "  服务:   $${state:-未知}（pid $${pid:-无}，KeepAlive 崩溃自动拉活）"; \
	else \
		echo "  服务:   未加载 —— make start 启动"; \
	fi
	@if curl -fsS -m 3 -o /dev/null -H "Host: 127.0.0.1:$(GATEWAY_PORT)" \
		"http://127.0.0.1:$(GATEWAY_PORT)/api/health" 2>/dev/null; then \
		echo "  本机:   OK  http://127.0.0.1:$(GATEWAY_PORT)"; \
	else \
		echo "  本机:   不可达（服务未运行或健康检查失败）"; \
	fi
	@lan=$$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null); \
		[ -n "$$lan" ] && echo "  局域网: http://$$lan:$(GATEWAY_PORT)（手机连同一 Wi-Fi 访问）" || true
	@url=$$(python3 -c "import json;c=json.load(open('$(GATEWAY_DATA_DIR)/config.json'));print(next((x.get('publicUrl') for x in c.get('connections',[]) if x.get('publicUrl')),''))" 2>/dev/null); \
		if [ -n "$$url" ]; then \
			if curl -fsS -m 5 -o /dev/null "$$url" 2>/dev/null; then echo "  外网:   OK  $$url"; \
			else echo "  外网:   不可达  $$url —— 检查 cloudflared 与隧道日志"; fi; \
		fi
	@grep "agents enabled" $(LAUNCHD_LOG) 2>/dev/null | tail -1 | sed 's/^[0-9/: ]*//; s/^/  Agent:  /' || true
	@echo "  日志:   $(LAUNCHD_LOG)（make logs 跟随）"

logs:
	@tail -n $${LINES:-50} -F $(LAUNCHD_LOG) 2>/dev/null || echo "暂无日志（make start 后生成于 $(LAUNCHD_LOG)）"

# 卸载 launchd 服务并删除 plist（保留数据目录与二进制）。
uninstall: stop
	@rm -f $(LAUNCHD_PLIST)
	@echo "已删除 $(LAUNCHD_PLIST)"

# HTTP 差分（Go 实现对拍已冻结的 golden；Python oracle 已随迁移退役，
# golden 变更需手工维护 conformance/http/golden.jsonl）。
conformance:
	python3 conformance/http/run.py --verify --target go
