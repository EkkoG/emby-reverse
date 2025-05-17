# emby-reverse

一个用于 Emby 的反向代理，可以自定义并注入虚拟媒体库、修改 API 响应、为媒体库提供自定义图片。项目使用 Go 编写，便于部署和与现有 Emby 服务器集成。

## 功能特性

- 在 Emby 视图中注入虚拟媒体库
- 支持 Docker 部署
- 支持 YAML 文件配置

## 配置说明

在项目根目录下创建 `config.yaml` 文件：

```yaml
emby_server: http://192.168.33.120:8096
library:
  - name: 所有电影
    collection_id: 8960
    image: ./images/movie.png
  - name: 所有电视剧
    collection_id: 8961
    image: ./images/tv.png
```

- `emby_server`：你的 Emby 服务器地址
- `library`：要注入的虚拟媒体库列表，每个库需包含：
  - `name`：媒体库显示名称, 须唯一
  - `collection_id`：Emby 实际集合 ID
  - `image`：该库的图片文件路径（用于自定义图片服务）

## 构建与运行

### 本地（Go 方式）

1. 安装 Go 1.22 或更高版本
2. 安装依赖：
   ```bash
   go mod tidy
   ```
3. 编译：
   ```bash
   go build -o emby-reverse main.go
   ```
4. 运行：
   ```bash
   ./emby-reverse
   ```

### Docker

1. 构建镜像：
   ```bash
   docker build -t emby-reverse .
   ```
2. 运行容器：
   ```bash
   docker run -d -p 8000:8000 --name emby-reverse emby-reverse
   ```

## Docker Compose 部署

你可以使用 Docker Compose 更方便地管理和部署 emby-reverse 服务。以下是一个示例 `docker-compose.yml` 文件：

```yaml
version: '3.8'
services:
  emby-reverse:
    image: ghcr.io/ekkog/emby-reverse:main
    container_name: emby-reverse
    ports:
      - "8000:8000"
    volumes:
      - ./config.yaml:/app/config.yaml
      - ./images:/app/images
    restart: unless-stopped
```

### 使用步骤

1. 确保 `config.yaml` 和 `images` 目录已在项目根目录下准备好。
2. 构建并启动服务：
   ```bash
   docker compose up -d
   ```
3. 查看日志：
   ```bash
   docker compose logs -f
   ```
4. 停止服务：
   ```bash
   docker compose down
   ```

### 说明

- `volumes` 用于挂载本地的配置文件和图片目录到容器内，便于自定义和持久化。
- `restart: unless-stopped` 保证服务异常退出后自动重启。
- 如需自定义端口或其他参数，可自行修改 `docker-compose.yml`。

## Nginx 反向代理示例

只将需要修改的 API 反代到本程序，其他 API 直接转发到原 Emby 服务器：

```nginx
upstream emby_reverse {
    server 127.0.0.1:8000;
}

upstream emby_origin {
    server 192.168.33.120:8096;
}

server {
    listen 80;
    server_name your.domain.com;

    # 需要 emby-reverse 处理的 API
    location ~ ^/emby/(Users|Items|Views) {
        proxy_pass http://emby_reverse;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # 其他 API 直接转发到 Emby
    location /emby/ {
        proxy_pass http://emby_origin;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## 常见问题

**Q: 虚拟媒体库的 ID 如何生成？**  
A: ID 是媒体库名称的 FNV-1a 哈希值（字符串）。

**Q: 支持哪些图片格式？**  
A: 只要 Go 的 `os.ReadFile` 能读取并作为字节流返回的图片格式都支持（如 PNG、JPG 等）。

**Q: 如何添加或删除媒体库？**  
A: 编辑 `config.yaml`，然后重启程序或容器。

**Q: 如何查看日志？**  
A: 程序日志输出到标准输出。Docker 方式可用 `docker logs emby-reverse` 查看。

## License

MIT 