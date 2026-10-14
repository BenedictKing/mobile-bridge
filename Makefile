# Go 网关（docs/go-migration-plan.md 已收官；Python 实现已退役删除）。
# 日常入口：make start / make stop / make restart / make status / make logs。

PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

LAUNCHD_LABEL := com.mobile-bridge.gateway
LAUNCHD_PLIST := $(HOME)/Library/LaunchAgents/$(LAUNCHD_LABEL).plist
LAUNCHD_LOG := $(HOME)/Library/Logs/mobile-bridge-gateway.log
GATEWAY_DATA_DIR ?= $(CURDIR)/.local
GATEWAY_PORT ?= 8787

.PHONY: build-matrix build-gateway-package build-wails serve \
	start stop restart status logs uninstall conformance

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
start: build-gateway-package
	@mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	@sed -e "s|@CURDIR@|$(CURDIR)|g" -e "s|@HOME@|$(HOME)|g" \
		deploy/launchd/gateway.plist.template > $(LAUNCHD_PLIST)
	@launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true
	@launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST)
	@launchctl kickstart gui/$$(id -u)/$(LAUNCHD_LABEL)
	@sleep 1
	@launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | grep -E "state =|pid =" | head -2
	@echo "$(LAUNCHD_LABEL) 已启动（KeepAlive 常驻）。日志: make logs"

# 彻底停止：bootout 卸载服务，KeepAlive 不会复活；下次 make start 再拉起。
stop:
	@launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null && echo "$(LAUNCHD_LABEL) 已停止并卸载" || echo "$(LAUNCHD_LABEL) 未在运行"
	@pkill -f "codex-mobile-gateway serve" 2>/dev/null || true

restart: stop start

status:
	@launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | grep -E "state =|pid =|last exit code" | head -4 || echo "$(LAUNCHD_LABEL) 未加载（make start 启动）"
	@curl -fsS -m 3 -o /dev/null -H "Host: 127.0.0.1:$(GATEWAY_PORT)" \
		"http://127.0.0.1:$(GATEWAY_PORT)/api/health" \
		&& echo "gateway health: OK (127.0.0.1:$(GATEWAY_PORT))" \
		|| echo "gateway health: 不可达"

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
