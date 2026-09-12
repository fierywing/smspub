# 构建阶段：静态编译（CGO 关闭），产物无 glibc 依赖，可直接跑在 alpine。
FROM golang:1.27-alpine AS build
WORKDIR /src

# 先只拷依赖清单，利用层缓存：源码改动不必重下模块。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# cmd/smspub 通过 embed.go 嵌入 web/，必须在 COPY . . 之后构建。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/smspub ./cmd/smspub

# 运行阶段：alpine 的 dialout 组恰为 gid 20，与 Debian 宿主机一致，
# 容器内以该组身份即可读写 /dev/ttyUSB*。
FROM alpine:3.20
RUN apk add --no-cache tzdata ca-certificates

# 以宿主机用户身份运行（uid/gid 1000），否则挂载卷里写出的文件属 root，
# 宿主机侧无法读写；dialout 组沿用 alpine 的 gid 20（与 Debian 一致），
# 用于读写 /dev/ttyUSB*。
RUN addgroup -g 1000 -S smspub 2>/dev/null || true; \
    adduser -u 1000 -S -G smspub -G dialout smspub 2>/dev/null || true

WORKDIR /app
COPY --from=build /out/smspub /app/smspub
# 前端已编译进二进制（embed.go），无需再拷 web/。

# conf/ 与 data/ 由 compose 卷挂入；先建好并交给运行用户。
RUN mkdir -p /app/data /app/conf && chown -R 1000:1000 /app

USER smspub
EXPOSE 8080
ENTRYPOINT ["/app/smspub"]
CMD ["-c", "/app/conf/config.json"]
