# 使用官方 Golang 镜像作为构建环境
FROM golang:1.22-alpine AS builder

WORKDIR /app

# 复制 go.mod 和 go.sum 并下载依赖
COPY go.mod go.sum ./
RUN go mod download

# 复制源代码
COPY . .

# 构建可执行文件
RUN go build -o emby-virtual-lib main.go

# 使用更小的基础镜像运行
FROM ghcr.io/astral-sh/uv:python3.12-alpine

WORKDIR /app

COPY . .

RUN mkdir -p mediacovergenerator
RUN cd mediacovergenerator && ln -s ../justzerock-mp-plugin/plugins.v2/mediacovergenerator/style_multi_1.py style_multi_1.py

# 拷贝可执行文件和配置、图片等资源
COPY --from=builder /app/emby-virtual-lib .

# 暴露端口
EXPOSE 8000

# 启动服务
CMD ["./emby-virtual-lib"] 