# prism-proxy 多阶段构建镜像
# 基础镜像与构建模式参考 ops-pilot deploy/server.Dockerfile
# （harbor 内网源 + 国内 GOPROXY，可通过 --build-arg 覆盖）

# ---------- 构建阶段 ----------
FROM harbor.cloud.netease.com/qzlowcode/golang:1.25.11-bookworm AS builder

# 适配受限网络环境（docker build --build-arg 可覆盖）
ARG GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy,direct
ARG GOSUMDB=off
ENV GOPROXY=$GOPROXY GOSUMDB=$GOSUMDB

# 版本信息（注入 main.version，对应 Makefile 的 LDFLAGS）
ARG VERSION=dev

WORKDIR /app

# 先复制依赖清单，利用层缓存
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 复制源码并构建（静态链接，无 CGO）
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /app/prism-proxy ./cmd/prism-proxy

# ---------- 运行阶段 ----------
FROM harbor.cloud.netease.com/qzlowcode/debian:bookworm-slim

# 换国内源 + 安装证书（HTTPS 出站必需）与时区数据
RUN sed -i 's|http://deb.debian.org/debian|http://mirrors.aliyun.com/debian|g; s|http://deb.debian.org/debian-security|http://mirrors.aliyun.com/debian-security|g' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata && \
    ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone && \
    rm -rf /var/lib/apt/lists/*

ENV TZ=Asia/Shanghai

WORKDIR /root/
COPY --from=builder /app/prism-proxy /usr/local/bin/prism-proxy

# 默认监听端口（make docker-run 映射到宿主机）
EXPOSE 8787

# 缺省读取 ~/.prism-proxy/settings.yaml（容器内为 /root/.prism-proxy/settings.yaml，
# 由 make docker-run 挂载宿主 ~/.prism-proxy 目录）
CMD ["prism-proxy", "serve"]
