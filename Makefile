# Go 网关（docs/go-migration-plan.md 已收官；Python 实现已退役删除）。
# 日常入口：make start / make stop / make restart / make status / make logs。
# 裸 make（默认目标）＝ make help。

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

.DEFAULT_GOAL := help

.PHONY: help build-matrix build-gateway build-gateway-package build-agent-host build-wails serve \
	install dev-cert start start-agent-host stop stop-agent-host restart status logs uninstall conformance

# 各目标行内 ## 注释即帮助文案，此处自动汇总。
help: ## 显示本帮助（make 默认目标）
	@awk -F'##' '/^[a-zA-Z_-]+:.*##/ {sub(/:.*/, "", $$1); sub(/^ +/, "", $$2); printf "  make %-24s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build-matrix: ## 交叉编译全平台网关二进制到 dist/
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags='-s -w' \
			-o dist/gateway-$$os-$$arch ./cmd/gateway || exit 1; \
	done
	@ls -lh dist/ | grep gateway-

build-gateway: ## 编译本机调试二进制 dist/gateway-go
	go build -o dist/gateway-go ./cmd/gateway

# The Wails desktop shell expects this stable runtime directory/name. The Go
# binary serves the gateway and exposes the private desktop action protocol.
build-gateway-package: ## 编译 serve/start 所用网关包（Wails 运行时固定目录名）
	@mkdir -p dist/gateway
	@go build -trimpath -ldflags='-s -w' -o dist/gateway/codex-mobile-gateway ./cmd/gateway
	@chmod 755 dist/gateway/codex-mobile-gateway
	@ls -lh dist/gateway/codex-mobile-gateway

build-agent-host: ## 编译独立 ACP Agent Host
	@mkdir -p dist/gateway
	@go build -trimpath -ldflags='-s -w' -o dist/gateway/mobile-bridge-agent-host ./cmd/agent-host
	@chmod 755 dist/gateway/mobile-bridge-agent-host
	@ls -lh dist/gateway/mobile-bridge-agent-host

start-agent-host: build-agent-host ## 启动并托管 ACP Agent Host（默认 Kimi）
	@mkdir -p $(dir $(ACP_HOST_INSTALL_BIN)) $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	@cp dist/gateway/mobile-bridge-agent-host $(ACP_HOST_INSTALL_BIN)
	@chmod 755 $(ACP_HOST_INSTALL_BIN)
	@$(call sign-step,$(ACP_HOST_INSTALL_BIN))
	@sed -e "s|@HOME@|$(HOME)|g" -e "s|@ACP_HOST_LABEL@|$(ACP_HOST_LABEL)|g" \
		-e "s|@ACP_HOST_SOCKET@|$(ACP_HOST_SOCKET)|g" -e "s|@ACP_HOST_BIN@|$(ACP_HOST_BIN)|g" \
		-e "s|@ACP_HOST_ARG@|$(ACP_HOST_ARG)|g" -e "s|@ACP_HOST_LOG@|$(ACP_HOST_LOG)|g" \
		deploy/launchd/agent-host.plist.template > $(ACP_HOST_PLIST)
	@launchctl bootout gui/$$(id -u)/$(ACP_HOST_LABEL) 2>/dev/null || true
	@for i in 1 2 3; do \
		launchctl bootstrap gui/$$(id -u) $(ACP_HOST_PLIST) && break; \
		[ $$i = 3 ] && exit 1; \
		echo "bootstrap 未就绪，重试（$$i/3）"; sleep 1; \
	done
	@launchctl kickstart gui/$$(id -u)/$(ACP_HOST_LABEL)
	@sleep 1
	@echo "$(ACP_HOST_LABEL) 已启动（socket $(ACP_HOST_SOCKET)）"

# Wails is the Go desktop shell. It embeds wails/frontend and calls the Go
# gateway action protocol directly.
build-wails: ## 编译 Wails 桌面外壳
	@mkdir -p dist/desktop
	@cd wails && go build -trimpath -ldflags='-s -w' -o ../dist/desktop/mobile-bridge-wails .
	@ls -lh dist/desktop/mobile-bridge-wails

# 固定路径 + 稳定签名身份。macOS TCC 按签名身份授予文稿/文件目录访问：重编译会
# 改变 ad-hoc cdhash，导致每次重启后重新弹"允许访问文稿文件夹"（Gateway 与
# Agent Host 同理）。故两个二进制都安装到固定路径并统一重签，优先用 keychain 里的
# mobile-bridge-dev 证书（make dev-cert 一次性生成），CODESIGN_IDENTITY=xxx 可显式覆盖。
# 注意：自签证书不会出现在 security find-identity -v 的列表里（未受信任），
# 但 codesign 可以直接使用，故以"尝试签名、失败再退 ad-hoc"探测。
# 单行 define：多行宏在 recipe 里会被 make 拆成多条 shell 导致 if/fi 断裂。
define sign-step
if [ -n "$(CODESIGN_IDENTITY)" ]; then codesign --force --sign "$(CODESIGN_IDENTITY)" "$(1)" && identity='$(CODESIGN_IDENTITY)'; elif codesign --force --sign mobile-bridge-dev "$(1)" 2>/dev/null; then identity='mobile-bridge-dev'; else codesign --force --sign - "$(1)" && identity='ad-hoc'; fi; echo "$(1) signed (identity: $$identity)"
endef

install: build-gateway-package ## 安装到 ~/.local/bin/mobile-bridge-gateway 并签名
	@mkdir -p $(dir $(INSTALL_BIN))
	@cp dist/gateway/codex-mobile-gateway $(INSTALL_BIN)
	@$(call sign-step,$(INSTALL_BIN))

# 一次性生成本机开发签名证书并导入默认钥匙串（10 年）。此后 make install 与
# make start-agent-host 自动以它签名，TCC 目录授权跨重编译稳定。
# 移除：security delete-certificate -c mobile-bridge-dev。
# 自签证书不受系统信任、不出现在 find-identity 列表，属预期；codesign 可直接使用。
dev-cert: ## 一次性生成本机签名证书 mobile-bridge-dev（稳定 TCC 身份）
	@if security find-certificate -c mobile-bridge-dev >/dev/null 2>&1; then \
		echo 'mobile-bridge-dev 已存在，跳过'; exit 0; \
	fi
	@tmp=$$(mktemp -d); \
	printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=critical,codeSigning\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid:always\n' > $$tmp/ext.cnf; \
	openssl req -new -newkey rsa:2048 -nodes -subj '/CN=mobile-bridge-dev/O=mobile-bridge' \
		-keyout $$tmp/mb.key -out $$tmp/mb.csr >/dev/null 2>&1 && \
	openssl x509 -req -days 3650 -in $$tmp/mb.csr -signkey $$tmp/mb.key \
		-extfile $$tmp/ext.cnf -out $$tmp/mb.crt >/dev/null 2>&1 && \
	security import $$tmp/mb.key -T /usr/bin/codesign && \
	security import $$tmp/mb.crt && \
	rm -rf $$tmp && \
	echo '已生成 mobile-bridge-dev（10 年）并导入登录钥匙串，下次 make install 起以该身份签名'

# 前台运行（调试用；生产请用 make start 走 launchd 常驻）。
# GATEWAY_DATA_DIR / GATEWAY_PORT 可覆盖以隔离验证。
serve: build-gateway-package ## 前台运行（调试用）
	@GATEWAY_PORT=$(GATEWAY_PORT) ./dist/gateway/codex-mobile-gateway serve --data-dir "$(GATEWAY_DATA_DIR)"

# launchd 常驻：写入 plist、加载并启动（KeepAlive 自动拉活）。
# 幂等——已加载时先卸载再以当前二进制重启。
start: install start-agent-host ## 编译+安装+启动 Gateway 与 ACP Agent Host
	@mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	@sed -e "s|@CURDIR@|$(CURDIR)|g" -e "s|@HOME@|$(HOME)|g" \
		-e 's|@ACP_HOST_SOCKETS@|$(ACP_HOST_SOCKETS_JSON)|g' \
		deploy/launchd/gateway.plist.template > $(LAUNCHD_PLIST)
	@launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true
	@for i in 1 2 3; do \
		launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST) && break; \
		[ $$i = 3 ] && exit 1; \
		echo "bootstrap 未就绪，重试（$$i/3）"; sleep 1; \
	done
	@launchctl kickstart gui/$$(id -u)/$(LAUNCHD_LABEL)
	@sleep 1
	@pid=$$(launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | awk -F' = ' '/^\tpid = /{print $$2; exit}'); \
		echo "$(LAUNCHD_LABEL) 已启动（pid $${pid:-?}，KeepAlive 常驻，崩溃自动拉活）"; \
		echo "日志: make logs   状态: make status"

# 彻底停止：bootout 卸载服务，KeepAlive 不会复活；下次 make start 再拉起。
stop-agent-host: ## 停止 ACP Agent Host
	@launchctl bootout gui/$$(id -u)/$(ACP_HOST_LABEL) 2>/dev/null && echo "$(ACP_HOST_LABEL) 已停止并卸载" || echo "$(ACP_HOST_LABEL) 未在运行"
	@pkill -f "mobile-bridge-agent-host --socket" 2>/dev/null || true

stop: stop-agent-host ## 停止并卸载 Gateway 与 ACP Agent Host
	@launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null && echo "$(LAUNCHD_LABEL) 已停止并卸载" || echo "$(LAUNCHD_LABEL) 未在运行"
	@pkill -f "(codex-mobile|mobile-bridge)-gateway serve" 2>/dev/null || true

restart: stop start ## 重新编译部署（日常更新入口）

status: ## 查看服务状态与本机/局域网/外网可达性
	@echo "== mobile-bridge 网关 =="
	@if launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) >/dev/null 2>&1; then \
		state=$$(launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | awk -F' = ' '/^\tstate = /{print $$2; exit}'); \
		pid=$$(launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null | awk -F' = ' '/^\tpid = /{print $$2; exit}'); \
		echo "  服务:   $${state:-未知}（pid $${pid:-无}，KeepAlive 崩溃自动拉活）"; \
	else \
		echo "  服务:   未加载 —— make start 启动"; \
	fi
	@if launchctl print gui/$$(id -u)/$(ACP_HOST_LABEL) >/dev/null 2>&1; then echo "  AgentHost: running（$(ACP_HOST_KIND)，socket $(ACP_HOST_SOCKET)）"; else echo "  AgentHost: 未加载"; fi
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

logs: ## 跟随网关日志（LINES=n 控制回看行数）
	@tail -n $${LINES:-50} -F $(LAUNCHD_LOG) 2>/dev/null || echo "暂无日志（make start 后生成于 $(LAUNCHD_LOG)）"

uninstall: stop ## 卸载服务并删除 plist（保留数据与二进制）
	@rm -f $(LAUNCHD_PLIST)
	@rm -f $(ACP_HOST_PLIST)
	@echo "已删除 $(LAUNCHD_PLIST)"

# Go 实现对拍已冻结的 golden；Python oracle 已随迁移退役，
# golden 变更需手工维护 conformance/http/golden.jsonl。
conformance: ## HTTP 差分对拍 golden
	python3 conformance/http/run.py --verify --target go
