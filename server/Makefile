# THVV Server —— 常用命令
BINARY := bin/thvv-server
CONFIG ?= ./config/thvv.local.yaml

.PHONY: help build run dev check test test-race fmt vet clean tidy

help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## 编译为 bin/thvv-server
	@mkdir -p bin
	go build -trimpath -ldflags "-s -w -X main.version=$${VERSION:-1.0.1} -X main.commit=$$(git rev-parse --short HEAD 2>/dev/null || echo dev)" -o $(BINARY) ./cmd/thvv-server

run: build ## 用指定配置启动（CONFIG=...）
	@test -n "$$THVV_SECRET_KEY" || { echo "请先 export THVV_SECRET_KEY=$$(openssl rand -base64 32)"; exit 1; }
	./$(BINARY) -config $(CONFIG)

dev: ## 本地联调（内置模拟引擎，无需 Python）
	@test -n "$$THVV_SECRET_KEY" || { echo "请先 export THVV_SECRET_KEY=$$(openssl rand -base64 32)"; exit 1; }
	go run ./cmd/thvv-server -config ./config/thvv.local.yaml -log-level debug

check: build ## 校验配置与运行环境
	@test -n "$$THVV_SECRET_KEY" || { echo "请先 export THVV_SECRET_KEY"; exit 1; }
	./$(BINARY) -config $(CONFIG) -check

test: ## 跑单元/集成测试
	go test ./...

test-race: ## 竞态检测
	go test -race ./...

fmt: ## 格式化
	gofmt -w ./cmd ./internal && gofmt -l ./cmd ./internal

vet: ## 静态检查
	go vet ./...

tidy: ## 整理依赖
	go mod tidy

clean: ## 清理产物
	rm -rf bin data
