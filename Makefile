# Go 迁移配套（docs/go-migration-plan.md P0/P1 交付物）。现有 Python 测试不受影响。

# P7 分发矩阵：五平台交叉编译（本地可验证的部分）
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build-matrix build-gateway-package build-wails
build-matrix:
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags='-s -w' \
			-o dist/gateway-$$os-$$arch ./cmd/gateway || exit 1; \
	done
	@ls -lh dist/ | grep gateway-

.PHONY: parity-goldens test-go parity build-gateway audit conformance

# 重新生成全部差分场景与 Python oracle goldens（改契约后必须重跑）
PARITY_DIRS = timeline ipc auth lifecycle model pairing store acpview acpadapter service projection timelineread submissions notifications historyexp

parity-goldens:
	@for dir in $(PARITY_DIRS) acp; do \
		if [ -f conformance/$$dir/gen_scenarios.py ]; then python3 conformance/$$dir/gen_scenarios.py; fi; \
		python3 conformance/$$dir/oracle.py --goldens; \
	done

test-go:
	go test ./...

# Go/Python 字节对拍（P1/P2 验收门槛）
parity: test-go

build-gateway:
	go build -o dist/gateway-go ./cmd/gateway

# Electron's extraResources expects this stable runtime directory/name. The
# Go binary itself serves the gateway; desktop control actions remain outside
# this target and are wired by the host controller.
build-gateway-package:
	@mkdir -p dist/gateway
	@go build -trimpath -ldflags='-s -w' -o dist/gateway/codex-mobile-gateway ./cmd/gateway
	@chmod 755 dist/gateway/codex-mobile-gateway
	@ls -lh dist/gateway/codex-mobile-gateway

# Wails is the Go desktop shell. It embeds wails/frontend and calls the Go
# gateway action protocol directly; Electron remains only for the legacy Python
# desktop path while the Go platform uses this target.
build-wails:
	@mkdir -p dist/desktop
	@cd wails && go build -trimpath -ldflags='-s -w' -o ../dist/desktop/mobile-bridge-wails .
	@ls -lh dist/desktop/mobile-bridge-wails

# 742 个测试的三分类清单（docs/go-migration-test-audit.md 的数据源）
audit:
	python3 conformance/classify_tests.py > /dev/null && echo "audit ok"

# HTTP 差分：同一份 golden 验收两个实现
conformance:
	python3 conformance/http/run.py --verify
	python3 conformance/http/run.py --verify --target go
