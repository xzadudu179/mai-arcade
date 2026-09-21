# mai-arcade —— 舞萌 DX 机台协议工具
#
# 常用目标：
#   make build     本地构建单二进制
#   make test      跑全部单测（含 -race，不访问网络）
#   make lint      gofmt 检查 + go vet
#   make release   交叉编译 linux/amd64 与 linux/arm64
#   make integration  跑需要真实网络的集成测试（需凭证的用例会自动跳过）
#   make clean     清理构建产物

BINARY      := mai-arcade
SERVER      := mai-arcade-server
CMD         := ./cmd/mai-arcade
SERVER_CMD  := ./cmd/mai-arcade-server
BIN_DIR     := bin
GO          ?= go
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w

# 交叉编译目标：方便把服务搬到树莓派或路由器旁的设备，换掉被阻断的出口 IP。
PLATFORMS   := linux/amd64 linux/arm64

.PHONY: all build test test-race test-isolated lint vet fmt vet-all integration release clean run-probe tidy

all: lint test build

## build: 构建本机二进制到 bin/（CLI 与服务两个）
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(SERVER) $(SERVER_CMD)
	@echo "已构建 $(BIN_DIR)/$(BINARY) 与 $(BIN_DIR)/$(SERVER)"

## test: 跑全部单测，不访问网络
test:
	$(GO) test ./...

## test-isolated: 在无网络的命名空间里跑单测
## 这是验证「单测不访问真实接口」这条纪律的直接手段；unshare 不可用时跳过。
test-isolated:
	@if unshare -rn true 2>/dev/null; then \
		unshare -rn sh -c 'ip link set lo up 2>/dev/null; $(GO) test -count=1 ./...'; \
	elif unshare -n true 2>/dev/null; then \
		unshare -n sh -c 'ip link set lo up 2>/dev/null; $(GO) test -count=1 ./...'; \
	else \
		echo "unshare 不可用，跳过隔离测试（请人工确认单测未访问真实接口）"; \
	fi

## test-race: 带竞态检测跑单测；并发相关的代码必须过这一关
test-race:
	$(GO) test -race ./...

## lint: 格式检查与静态检查
lint: fmt vet

## fmt: 检查格式（只报告不修改，便于 CI 判定）
fmt:
	@out="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$out" ]; then echo "以下文件未格式化："; echo "$$out"; exit 1; fi
	@echo "格式检查通过"

## vet: 静态检查，含 integration 标签下的文件
vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

## integration: 跑需要真实网络的集成测试
## 需要凭证的用例会自行跳过，可这样显式启用：
##   MAI_SGID='SGWCMAID...' MAI_FISH_TOKEN='...' make integration
integration:
	$(GO) test -tags integration -count=1 ./... -v

## release: 交叉编译到多平台
release:
	@mkdir -p $(BIN_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=$(BIN_DIR)/$(BINARY)-$$os-$$arch; \
		echo "构建 $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(CMD) || exit 1; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $$out-server $(SERVER_CMD) || exit 1; \
	done
	@ls -lh $(BIN_DIR)

## run-probe: 构建后立即做一次连通性自检
run-probe: build
	$(BIN_DIR)/$(BINARY) probe; echo "退出码=$$?"

## tidy: 整理依赖（本项目应为零第三方依赖）
tidy:
	$(GO) mod tidy
	@echo "--- go.mod ---"; cat go.mod

## clean: 清理构建产物
clean:
	rm -rf $(BIN_DIR)

## help: 列出可用目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
