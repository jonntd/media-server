# Media Server 302
### 使用 Docker Compose 运行容器

在项目根目录下创建 `docker-compose.yml` 文件，内容如下：

```yaml
version: '3.8'
services:
  cloudnas:
    image: cloudnas/clouddrive2
    container_name: clouddrive2
    environment:
      - TZ=Asia/Shanghai
      - CLOUDDRIVE_HOME=/Config
    volumes:
      - /data/media-server/cloud2/CloudNAS:/CloudNAS:shared
      - /data/media-server/cloud2/Config:/Config
      - /data/media-server/cloud2/media:/media:shared 
    devices:
      - /dev/fuse:/dev/fuse
    ports:
      - "19798:19798"
    restart: unless-stopped
    pid: "host"
    privileged: true

  emby_server:
    image: "emby/embyserver_arm64v8:latest"
    # image: "emby/embyserver:latest"
    container_name: "emby_server"
    restart: always
    ports:
      - "8096:8096"
    volumes:
      - /data/media-server/emby:/config
      - /data/media-server/cloud2/CloudNAS/CloudDrive:/media
    environment:
      - "TZ=Asia/Shanghai"
    networks:
      - internal_network
  media-server:
    container_name: "media-server"
    image: "ghcr.io/jonntd/media-server:latest"
    ports:
      - "9096:9096"
    volumes:
      - /data/media-server/config.yaml:/config.yaml
      - /data/media-server/logs:/logs
    networks:
      - internal_network
    environment:
      - "TZ=Asia/Shanghai"
networks:
  internal_network:
    driver: bridge
```


115-cookies.txt  浏览器不大助手获取
```
UID=; CID=; SEID=
```
config.yaml 

```yaml
server:
  # 替换成自己的挂载路径
  # 如果你的 Emby 运行在 Windows 下，可以向下面这样填 mount-page: "D:/115(123456)" (大概是这样吧)
  mount-path: /media/115(123456)
  cookie: 115cookies

emby:
  url: http://emby_server:8096
  apikey: embyApi

```
