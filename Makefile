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

# 声明所有伪目标
.PHONY: build build-all test test-race vet fmt clean
