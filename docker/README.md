# Docker 使用说明

本分支目前不发布预构建镜像。请从当前仓库源码构建，避免拉取到不包含可靠性改动的上游镜像。

在仓库根目录运行：

```bash
docker compose -f docker/docker-compose.yml up --build -d
```

查看日志：

```bash
docker compose -f docker/docker-compose.yml logs -f
```

停止服务：

```bash
docker compose -f docker/docker-compose.yml stop
```

## 数据与本地图片

- `docker/data/` 持久化 Cookie、浏览器状态和异步任务状态。这里包含敏感信息，不要提交或分享。
- `docker/images/` 用于待发布图片。调用 MCP 时，容器内路径应写成 `/app/images/<文件名>`。
- 服务默认暴露在 `http://localhost:18060/mcp`。

## 鉴权

生产部署应设置随机且足够强的 Bearer Token：

```bash
AUTH_TOKEN=replace-with-a-strong-random-value \
  docker compose -f docker/docker-compose.yml up --build -d
```

客户端随后必须发送：

```text
Authorization: Bearer <token>
```

## 代理

如需代理，可在 `docker/docker-compose.yml` 的 `environment` 中增加：

```yaml
- XHS_PROXY=http://user:pass@proxy:port
```

支持 HTTP、HTTPS 和 SOCKS5。不要把带认证信息的代理地址提交到仓库。

## 风险

自动化操作可能导致账号功能受限或封禁。建议使用专门测试账号先完成真实验收，使用者自行承担相关风险。
