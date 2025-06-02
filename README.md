# emby-virtual-lib

A reverse proxy for Emby that allows you to customize and inject virtual libraries, modify API responses, and provide custom images for libraries. The project is written in Go for easy deployment and integration with existing Emby servers.

**English** · [简体中文](./README-zh.md)

## Features

- Inject virtual libraries into Emby views
- Support Docker deployment
- Support YAML configuration

## Configuration

Create a `config.yaml` file in the project root directory:

```yaml
emby_server: http://192.168.33.120:8096
log_level: info
emby_api_key: 1234567890
hide:
  - music
  - tvshows
  - movies
  - boxsets
  - playlists
library:
  - name: All Movies
    resource_id: 8960
    resource_type: collection
    image: ./images/movie.png
  - name: All TV Shows
    resource_id: 8961
    resource_type: collection
    image: ./images/tv.png
  - name: Tag
    resource_id: 10247
    resource_type: tag
  - name: Genre
    resource_id: 246
    resource_type: genre
  - name: Studio
    resource_id: 10242
    resource_type: studio
  - name: Actor
    resource_id: 10232
    resource_type: person
```

- `emby_server`: Your Emby server address
- `emby_api_key`: (optional, default: empty) If set, the program will fetch image from emby server automatically.
- `log_level`: (optional, default: info) Log level, options: `debug`, `info`, `warn`, `error`.
- `hide`: (optional, default: empty) If set, the program will hide the libraries in Emby views.
- `library`: List of virtual libraries to inject. Each library must include:
  - `name`: Display name of the library (must be unique)
  - `resource_id`: Resource id, the meaning of id is different according to resource_type
  - `resource_type`: Resource type, optional values: `collection`, `tag`, `genre`, `studio`, `person`
  - `image`: Path to the image file for this library (used for custom image service)

## Build & Run

### Local (Go)

1. Install Go 1.22 or higher
2. Install dependencies:
   ```bash
   go mod tidy
   ```
3. Build:
   ```bash
   go build -o emby-virtual-lib main.go
   ```
4. Run:
   ```bash
   ./emby-virtual-lib
   ```

### Docker

1. Build the image:
   ```bash
   docker build -t emby-virtual-lib .
   ```
2. Run the container:
   ```bash
   docker run -d -p 8000:8000 --name emby-virtual-lib emby-virtual-lib
   ```

## Docker Compose Deployment

You can use Docker Compose to manage and deploy emby-virtual-lib more easily. Here is a sample `docker-compose.yml` file:

```yaml
version: '3.8'
services:
  emby-virtual-lib:
    image: ghcr.io/ekkog/emby-virtual-lib:main
    container_name: emby-virtual-lib
    ports:
      - "8000:8000"
    volumes:
      - ./config.yaml:/app/config.yaml
      - ./images:/app/images
    restart: unless-stopped
```

### Usage Steps

1. Make sure `config.yaml` and the `images` directory are ready in the project root.
2. Build and start the service:
   ```bash
   docker compose up -d
   ```
3. View logs:
   ```bash
   docker compose logs -f
   ```
4. Stop the service:
   ```bash
   docker compose down
   ```

### Notes

- `volumes` mounts the local config file and images directory into the container for customization and persistence.
- `restart: unless-stopped` ensures the service restarts automatically if it exits unexpectedly.
- You can modify `docker-compose.yml` to customize ports or other parameters as needed.

## Nginx Reverse Proxy Example

Only the APIs that need to be modified are proxied to this program, other APIs are forwarded directly to the original Emby server:

```nginx
upstream emby_virtual_lib {
    server 127.0.0.1:8000;
}

upstream emby_origin {
    server 192.168.33.120:8096;
}

server {
    listen 80;
    server_name your.domain.com;

        # only proxy /emby/Users/<id>/Views、/Items、/Items/Latest to emby-virtual-lib
        location ~ /Users/[^/]+/(Views|Items|Items/Latest) {
                proxy_pass http://emby_virtual_lib;
                proxy_redirect          off;
                proxy_buffering         off;
                proxy_set_header        Host                    $host;
                proxy_set_header        X-Real-IP               $remote_addr;
                proxy_set_header        X-Forwarded-For         $proxy_add_x_forwarded_for;
                proxy_set_header        X-Forwarded-Protocol    $scheme;
        }

        # only proxy image to emby-virtual-lib
        location ~ /Items/[^/]+/Images/Primary {
                proxy_pass http://emby_virtual_lib;
                proxy_redirect          off;
                proxy_buffering         off;
                proxy_set_header        Host                    $host;
                proxy_set_header        X-Real-IP               $remote_addr;
                proxy_set_header        X-Forwarded-For         $proxy_add_x_forwarded_for;
                proxy_set_header        X-Forwarded-Protocol    $scheme;
        }

	location / {
		proxy_pass http://emby_origin;
                proxy_redirect          off;
                proxy_buffering         off;
                proxy_set_header        Host                    $host;
                proxy_set_header        X-Real-IP               $remote_addr;
                proxy_set_header        X-Forwarded-For         $proxy_add_x_forwarded_for;
                proxy_set_header        X-Forwarded-Protocol    $scheme;
	}
}
```

## FAQ

**Q: How is the virtual library ID generated?**  
A: The ID is the FNV-1a hash (string) of the library name.

**Q: What image formats are supported?**  
A: Any image format that Go's `os.ReadFile` can read and return as a byte stream is supported (e.g., PNG, JPG, etc.).

**Q: How to add or remove a library?**  
A: Edit `config.yaml`, then restart the program or container.

**Q: How to view logs?**  
A: The program outputs logs to standard output. For Docker, use `docker logs emby-virtual-lib` to view logs.

## License

MIT 