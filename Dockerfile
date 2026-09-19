# ============ 多阶段构建：在任意架构机器上 docker-compose build 都会自动编译出本机架构二进制 ============

# 第一阶段：编译（alpine 镜像自带 Go 工具链；CGO_ENABLED=0 纯静态，无 glibc 依赖）
FROM golang:1.27-alpine AS builder
WORKDIR /src
# GOPROXY 必须换国内源：默认 proxy.golang.org 在国内不可达，会报
# "tls: bad record MAC"（数据被中间设备改坏，不是缺依赖）。多家备选，任一可用即可。
ENV GOPROXY=https://goproxy.cn,https://goproxy.io,direct
ENV GOSUMDB=sum.golang.google.cn
# 先拷贝依赖清单，命中缓存可跳过 go mod download
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o mmbot ./cmd/mmbot

# 第二阶段：运行时（精简 alpine，只放产物）
FROM alpine:3.19
# tzdata：让 Go（time.Local）与 busybox date 都能加载 Asia/Shanghai，
# 否则容器内时间会回退到 UTC，日志时间比实际（北京时间）慢 8 小时
# docker-cli：TG /update 命令需要操作宿主 docker（配合挂载 /var/run/docker.sock 使用）
# docker-compose：镜像内置 compose v2（固定版本），/update 用它执行重建，
#   不依赖外部 docker/compose 镜像（其 latest 是 2020 年的 v1.26.2，解析不了现代 compose 文件）
# 多架构：buildx 构建时 TARGETARCH 自动注入（amd64/arm64/arm），按架构下载对应 compose 二进制
ARG TARGETARCH
RUN apk add --no-cache ca-certificates tzdata docker-cli && \
    case "$TARGETARCH" in \
      amd64) COMPOSE_ARCH=x86_64 ;; \
      arm64) COMPOSE_ARCH=aarch64 ;; \
      arm)   COMPOSE_ARCH=armv7 ;; \
      *) echo "unsupported arch: $TARGETARCH"; exit 1 ;; \
    esac && \
    wget -q -O /usr/local/bin/docker-compose https://github.com/docker/compose/releases/download/v2.29.7/docker-compose-linux-${COMPOSE_ARCH} && \
    chmod +x /usr/local/bin/docker-compose && \
    docker-compose version && \
    ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone
ENV TZ=Asia/Shanghai
COPY --from=builder /src/mmbot /app/
# 传统 docker build（如 NAS 通道）COPY 保留源权限，Windows/scp 上传的二进制无执行位，必须显式加
RUN chmod +x /app/mmbot
COPY static /app/static
COPY templates /app/templates
RUN mkdir -p /app/config
# 配置模板放在 /app/config 之外：宿主 ./config 挂载会遮蔽 /app/config，模板被遮就丢了
COPY templete.env /app/templete.env
WORKDIR /app
EXPOSE 8000
# 首次启动（./config 为空挂载）时自动生成 user.env，保证"只有 docker-compose.yml 也能跑"；
# 已有 user.env 不覆盖（保留用户在 Web 面板的配置，恢复备份同样生效）
# 每次启动都把镜像内置模板同步到挂载卷的 templete.env：宿主 ./config 卷是持久的，
# 若不回刷，/update 后磁盘上的模板还是旧版，导致 /api/env 返回的章节/key 落后于前端（如 PANFX 配置不显示）。
# 只同步模板，user.env 绝不动（用户配置值都在那里）。
CMD ["/bin/sh", "-c", "[ -f /app/config/user.env ] || cp /app/templete.env /app/config/user.env; cp /app/templete.env /app/config/templete.env; exec ./mmbot"]
