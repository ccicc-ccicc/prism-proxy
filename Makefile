# prism-proxy 构建脚本
# 支持本机构建、多平台交叉构建、测试与清理

# 项目信息
BINARY  := prism-proxy
CMD_PKG := ./cmd/prism-proxy

# 版本号：优先取 git 描述（tag/commit/工作区状态），无 git 时回退 dev
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# 链接参数：把版本号注入 main.version 变量
LDFLAGS := -X main.version=$(VERSION)

# 交叉构建矩阵（可用 BUILD_OS / BUILD_ARCH 覆盖，默认 6 组 os×arch）
BUILD_OS   ?= linux darwin windows
BUILD_ARCH ?= amd64 arm64

# 构建产物目录
BIN_DIR  := bin
DIST_DIR := dist

# 生成平台产物路径（windows 追加 .exe 后缀）
target_name = $(DIST_DIR)/$(BINARY)-$(1)-$(2)$(if $(filter windows,$(1)),.exe)

# 单个平台的构建规则模板
# 注意：build / build-all 依赖 clean（先删后建），不可并行（make -j）——
# 并行执行时 clean 可能删除其他目标正在写入的产物目录。
define build-rule
$(call target_name,$(1),$(2)):
	mkdir -p $(DIST_DIR)
	env GOOS=$(1) GOARCH=$(2) CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $$@ $(CMD_PKG)
endef

# 用 foreach 双重循环展开整个矩阵
$(foreach os,$(BUILD_OS),$(foreach arch,$(BUILD_ARCH),$(eval $(call build-rule,$(os),$(arch)))))

# build-all 的全部目标列表
BUILD_TARGETS := $(foreach os,$(BUILD_OS),$(foreach arch,$(BUILD_ARCH),$(call target_name,$(os),$(arch))))

# 默认目标
.DEFAULT_GOAL := build

# 本机构建：先 clean 再构建，避免残留旧产物
build: clean
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD_PKG)

# 多平台交叉构建：先 clean 再按矩阵构建
build-all: clean $(BUILD_TARGETS)

# 运行全部测试
test:
	go test ./...

# 竞态检测测试
test-race:
	go test -race ./...

# 静态检查
vet:
	go vet ./...

# 检查代码格式（仅列出未格式化文件，不修改）
fmt:
	gofmt -l .

# 清理全部构建产物
clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)

# ==================== Docker 镜像构建与运行 ====================

# 镜像名与标签（默认取版本号，可覆盖）
IMAGE_NAME ?= prism-proxy
IMAGE_TAG  ?= $(VERSION)

# 镜像仓库前缀（多架构 --push 时必填；单平台本地 --load 构建可留空）
REGISTRY ?=

# 构建平台：默认多架构（amd64+arm64）推送到仓库；
# 本地构建/运行请指定单平台：make docker-build PLATFORM=linux/amd64
PLATFORM ?= linux/amd64,linux/arm64

# buildx builder 名称（多架构需要 docker-container driver，首次自动创建）
BUILDX_BUILDER ?= prism-proxy-builder

# 容器运行配置
CONTAINER_NAME ?= prism-proxy
LISTEN_PORT    ?= 8787
SETTINGS_DIR   ?= $(HOME)/.prism-proxy

# 完整镜像引用（REGISTRY 为空时为本地镜像名）
IMAGE_REF := $(if $(REGISTRY),$(REGISTRY)/,)$(IMAGE_NAME):$(IMAGE_TAG)

# 一键构建 Docker 镜像（buildx 多架构）
# - 多架构（PLATFORM 含逗号）→ --push 推送到 REGISTRY（参考 ops-pilot 多平台构建）
# - 单平台 → --load 本地加载（供 make docker-run 使用）
docker-build:
	@docker buildx create --name $(BUILDX_BUILDER) 2>/dev/null || true
	@if echo "$(PLATFORM)" | grep -q ","; then \
		echo "构建多架构镜像（$(PLATFORM)）并推送 $(REGISTRY)/..."; \
		[ -n "$(REGISTRY)" ] || { echo "[ERROR] 多架构构建需要指定 REGISTRY，如：make docker-build REGISTRY=harbor.example.com/ns"; exit 1; }; \
		docker buildx build --builder $(BUILDX_BUILDER) --platform "$(PLATFORM)" --push \
			--build-arg VERSION=$(VERSION) \
			-t $(IMAGE_REF) .; \
	else \
		echo "构建单架构镜像（$(PLATFORM)）并本地加载"; \
		docker buildx build --builder $(BUILDX_BUILDER) --platform "$(PLATFORM)" --load \
			--build-arg VERSION=$(VERSION) \
			-t $(IMAGE_REF) .; \
	fi
	@echo "构建完成: $(IMAGE_REF)"

# 一键启动 Docker 容器服务：镜像不存在时先自动本地构建（单平台 --load）保证镜像存在；
# 启动前停旧容器；打印将要执行的 docker run 命令
docker-run:
	@if ! docker image inspect $(IMAGE_REF) >/dev/null 2>&1; then \
		echo "[提示] 镜像 $(IMAGE_REF) 不存在，先执行本地构建（make docker-build PLATFORM=linux/$(shell go env GOARCH)）..."; \
		$(MAKE) docker-build PLATFORM=linux/$(shell go env GOARCH); \
	fi
	@docker rm -f $(CONTAINER_NAME) 2>/dev/null || true
	@echo "启动 prism-proxy 容器，命令如下："
	@echo "  docker run -d --name $(CONTAINER_NAME) -p $(LISTEN_PORT):8787 -v $(SETTINGS_DIR):/root/.prism-proxy $(IMAGE_REF)"
	@docker run -d --name $(CONTAINER_NAME) \
		-p $(LISTEN_PORT):8787 \
		-v $(SETTINGS_DIR):/root/.prism-proxy \
		$(IMAGE_REF)
	@echo "prism-proxy 已启动: http://localhost:$(LISTEN_PORT)"
	@echo "查看日志: docker logs -f $(CONTAINER_NAME)"

# 声明所有伪目标
.PHONY: build build-all test test-race vet fmt clean docker-build docker-run
