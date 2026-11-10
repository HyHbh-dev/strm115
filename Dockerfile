# syntax=docker/dockerfile:1

# 全局 ARG 必须写在第一个 FROM 之前，否则后面 FROM 里的 ${...} 会展开成空字符串
ARG NODE_VERSION=22
ARG GO_VERSION=1.27

# ---------- 前端构建阶段 ----------
# Vue 版管理界面（web/）在这里编译，产物再拷进 Go 阶段做 go:embed。
# 单独成阶段是为了让「只改 Go 代码」时这一层还能命中缓存。
FROM node:${NODE_VERSION}-alpine AS web

WORKDIR /web

# 国内 npm 走镜像：默认 registry 在国内经常慢到超时
ARG NPM_REGISTRY=https://registry.npmmirror.com
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm config set registry "${NPM_REGISTRY}" \
 && npm ci --no-audit --no-fund

COPY web/ ./
# vite.config.ts 里 outDir 指向 ../internal/httpapi/webdist，
# 在这里就是 /internal/httpapi/webdist
RUN npm run build

# ---------- 构建阶段 ----------
# 只用官方 Go 镜像编译，产物是静态二进制，运行镜像不需要 Go。
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# 国内构建走代理。官方默认的 proxy.golang.org 在国内基本不通，
# 这个参数让「拉不下来依赖」不至于变成一个需要自己改 Dockerfile 的坑。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

# 依赖单独一层：只改业务代码时不会重下模块
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

# go:embed 要求编译时目录存在，所以前端的产物必须在这之前落地
COPY --from=web /internal/httpapi/webdist ./internal/httpapi/webdist

ARG VERSION=v0.5.2
# CGO_ENABLED=0 是关键：SQLite 用的是纯 Go 驱动（modernc.org/sqlite），
# 不依赖 libc，才能产出真正静态的二进制、扔进最小运行镜像。
# -trimpath 去掉本机路径，-s -w 去掉符号表和调试信息（19MB → 约 13MB）。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.Version=${VERSION}" \
        -o /out/strm115 ./cmd/server

# ---------- 运行阶段 ----------
FROM alpine:3.24

# VERSION 是在 build 阶段声明的 ARG，跨阶段要重新引入才能用在 LABEL 里
ARG VERSION=v0.5.2
# 发布到镜像仓库时补一行 org.opencontainers.image.source="https://…"，
# 部分仓库会据此把镜像关联回代码仓库页面。
LABEL org.opencontainers.image.title="strm115" \
      org.opencontainers.image.description="115 网盘目录 → STRM → Emby/Jellyfin，播放 302 直连 CDN" \
      org.opencontainers.image.version="${VERSION}"

# ca-certificates：访问 115 OpenAPI / Emby 的 HTTPS 必须有根证书
# tzdata：日志和台账时间戳用本地时区（RFC3339 带偏移）
# su-exec：entrypoint 里降权用（约 10KB）
RUN apk add --no-cache ca-certificates tzdata su-exec \
 && addgroup -S -g 10001 strm \
 && adduser -S -u 10001 -G strm -h /app strm

ENV TZ=Asia/Shanghai \
    DATA_DIR=/data \
    MEDIA_ROOT=/media \
    ADDR=:6060 \
    PROXY_ADDR=:6086

WORKDIR /app
COPY --from=build /out/strm115 /app/strm115
COPY docker/entrypoint.sh /entrypoint.sh
# 故意不预建 /media：它必须由用户挂载进来。
# 目录不存在时 entrypoint 会给出明确警告，好过把 .strm 悄悄写进容器层。
# sed 去 CR：Windows 上 git 的 autocrlf 会把 .sh 转成 CRLF，
# 进容器后第一行变成 "#!/bin/sh\r"，报错是 "not found"，极易误判成缺 shell。
# .gitattributes 已经约束了 LF，这里是最后一道保险。
RUN sed -i 's/\r$//' /entrypoint.sh && chmod +x /entrypoint.sh && mkdir -p /data

# 两个端口都要暴露：6060 管理后台，6086 播放代理（.strm 里的地址指向它）
EXPOSE 6060 6086

# 数据卷只声明 /data（SQLite + 会话密钥）。/media 故意不声明 VOLUME——
# 它是宿主上已有的媒体目录，必须由用户显式 bind mount，匿名卷会造成两份。
VOLUME ["/data"]

# 健康检查两个端口都要过：
#   6060 /api/session  —— 管理后台（唯一无需鉴权的接口）
#   6086 /healthz      —— 播放代理，这才是核心功能，它挂了容器不能算健康
# 端口从环境变量里解出来，改了监听端口也不需要改这里。
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${ADDR#*:}/api/session" \
     && wget -q -O /dev/null "http://127.0.0.1:${PROXY_ADDR#*:}/healthz" || exit 1

# 优雅退出：程序收到 SIGTERM 后会先用 10 秒收尾（等在途同步写完 .strm）
STOPSIGNAL SIGTERM

ENTRYPOINT ["/entrypoint.sh"]
