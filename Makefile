# Go 迁移配套（docs/go-migration-plan.md P0/P1 交付物）。现有 Python 测试不受影响。

.PHONY: parity-goldens test-go parity build-gateway audit conformance

# 重新生成全部差分场景与 Python oracle goldens（改契约后必须重跑）
PARITY_DIRS = timeline ipc auth lifecycle model pairing store acpview acpadapter service projection

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

# 742 个测试的三分类清单（docs/go-migration-test-audit.md 的数据源）
audit:
	python3 conformance/classify_tests.py > /dev/null && echo "audit ok"

# HTTP 差分：同一份 golden 验收两个实现
conformance:
	python3 conformance/http/run.py --verify
	python3 conformance/http/run.py --verify --target go
