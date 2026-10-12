# NAS / Docker 公网入口

适合已经用 NAS 反向代理 Docker 应用、已有域名和 HTTPS 证书的用户。无需 Cloudflare。

```text
手机 → https://codex.example.com
     → NAS 的 HTTPS 反向代理
     → 本目录的 Nginx 容器（可选）
     → 电脑的 http://192.168.1.10:8787
     → 同一电脑的 Codex App → 本机或 SSH 聊天
```

**Docker 部署的是访问入口。Codex App 和网关仍运行在原 Mac / Windows 电脑上。** 网关依赖同一用户的桌面 IPC、会话数据及 SSH 配置；把仓库单独装进 NAS 容器不能连接另一台电脑上的 App。不要把 `.codex`、API Key 或 SSH 私钥复制到 NAS。

NAS 必须能够访问电脑的局域网地址。两者不在同一网络时，需要先建立已有 VPN 或其他受保护的网络连接；家中 NAS 无法直接访问校园网内的电脑。电脑需要保持唤醒，Codex App、网关和所需 SSH 连接持续可用。

## 1. 电脑配置

电脑桌面启动器中：

1. “网络与登录”开启局域网访问，保留当前端口，保持账号密码登录。
2. 在“其他连接配置”添加“固定域名 · NAS / 已有反代 / Docker”，填写手机 HTTPS 地址和 NAS 可达的电脑 HTTP 地址。可点击“导出部署包”获取已填好地址的配置，或“复制给部署 Agent”。保存后重新启动网关。运行中的服务须重启才会使用新源；提前安排手机可短暂断开的时机。
3. 无需开启 Cloudflare 临时 HTTPS。
4. “手机通知”的通知跳转地址留空即可自动使用固定入口；若此前手动填过旧地址，请清空或改为新域名。

命令行部署也可以在原有启动参数后添加（示例为 macOS，Windows 用 `py -3 -B`）：

```sh
python3 -B run.py --lan --origin https://codex.example.com
```

替换示例域名和 IP；有非标准 HTTPS 端口时，允许的源、代理保留的 Host 和通知地址都要包含该端口。源不带路径或末尾 `/`。

## 2. 选择一种 NAS 配置

### A. 已有反代可以直接转发到电脑（最简单）

在群晖、威联通、Nginx Proxy Manager 等已有反代中添加：

| 设置 | 值 |
| --- | --- |
| 公网入口 | `https://codex.example.com:443` |
| 上游协议 | HTTP |
| 上游地址 | `192.168.1.10`（电脑 IP） |
| 上游端口 | `8787`（电脑网关端口） |
| Host 请求头 | 保留外部域名及端口，Nginx 为 `$http_host` |
| 超时 | 读取、发送均设为 300 秒 |
| 缓存 / 响应缓冲 | 关闭 |

域名使用独立子域名的根路径；当前网页不支持部署在 `/codex/` 等子路径下。网页使用 HTTP 上的增量 SSE 事件流，无需 WebSocket；每一层反代都需放行并且不缓冲 `text/event-stream`。登录和后续写请求的 `Origin`、`Cookie`、`X-CSRF-Token` 应原样传递。

如果这样已经可以访问，不需要增加容器。

### B. 希望按 Docker 应用统一管理入口

在 NAS 上下载仓库（默认分支 `main`），进入 `deploy/nas`：

```sh
cp .env.example .env
```

编辑 `.env` 中的 `GATEWAY_UPSTREAM`，填写电脑 IP 和网关端口，然后执行：

```sh
docker compose config
docker compose up -d
docker compose logs --tail=50
```

NAS 自带 HTTPS 反代的目标填 `http://127.0.0.1:18787`，并保留外部 Host。容器只负责转发；证书、域名和公网端口继续由现有反代管理。

如果外层反代也在 Docker 中，它的 `127.0.0.1` 指向自身容器。可以将 `BIND_ADDRESS` 改为 **NAS 的局域网 IP**，再把外层反代上游指向 `http://NAS局域网IP:18787`；仅让该反代访问此端口。不要把 `18787` 或电脑 `8787` 直接映射到公网。多个代理层都需保留公网 Host、关闭缓存并设置合适的超时。

停用此入口：

```sh
docker compose down
```

这不会停止电脑网关或影响原局域网地址。

## 3. 验收和排错

1. 从 NAS 确认电脑网关可达；若不通，检查电脑是否唤醒、网关是否开启局域网监听、防火墙和跨网络路由。
2. 手机关闭 Wi-Fi，使用蜂窝网络打开公网 HTTPS 域名。必须先看到登录页，未登录不能读取聊天。
3. 登录后确认列表、近期内容、上翻历史和实时更新；只在专用测试聊天中试发消息和授权。
4. 关注一个测试聊天，检查 ntfy 点击后跳转到这个固定域名的对应聊天。

| 现象 | 检查 |
| --- | --- |
| 502 / 504 | NAS 能否访问电脑 IP:端口；电脑是否休眠；容器是否把 localhost 当成电脑 |
| “此访问地址未在网关配置中允许” | 电脑配置是否包含完整 HTTPS 源并已重启；每一层反代是否保留 Host |
| “不允许跨站请求” | 地址栏域名与允许的源是否一致；是否错误改写 Origin |
| 登录后不断回到登录页 | 全程使用 HTTPS；代理是否保留 Cookie；禁用缓存 |
| 实时更新经常断开 | 外层和内层代理的超时；响应缓冲（SSE 要求不缓冲 `text/event-stream`）；不要缓存 `/api/` |
| 通知打开旧地址 | 设置通知跳转地址，再产生新通知；已收到通知里的旧链接不会更新 |

把网页直接部署到公网也意味着登录入口对公网开放。保留网关密码验证、HTTPS 和现有 NAS 访问控制；不需要公开任何 App IPC 端点。

## 给部署 Agent

先确认电脑网关地址、NAS 是否可达电脑、现有反代的位置及用户的 HTTPS 域名。已有反代能直连时用方案 A，否则用方案 B。不要改动原局域网端口、模型认证或 SSH 凭据；重启网关前安排访问中断时间。完成后返回公网 HTTPS 链接、保留的局域网链接、密码获取方式、配置位置、启停命令、实际验收结果及未验证项。没有 NAS 权限或公网入口时，交付准备好的配置，不声称已部署。

配置依据：[Docker Compose](https://docs.docker.com/compose/)、[官方 Nginx 镜像模板](https://github.com/nginx/docker-nginx/blob/master/entrypoint/20-envsubst-on-templates.sh)、[Nginx 代理模块](https://nginx.org/en/docs/http/ngx_http_proxy_module.html)。
