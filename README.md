# Xiaohongshu MCP Reliable

一个面向 AI Agent 自动化的小红书 MCP 可靠性分支，基于
[`xpzouying/xiaohongshu-mcp`](https://github.com/xpzouying/xiaohongshu-mcp)。

上游项目已经提供了登录、浏览、搜索、发布、评论、点赞、收藏与通知等完整能力；这个分支重点处理另一类问题：当浏览器超时、进程重启或模型重复调用时，如何避免重复发布、重复评论和回复错楼。

> 这是非官方社区分支，与小红书及上游维护者无隶属关系。自动化操作可能受站点规则和页面变化影响，请遵守适用的服务条款与法律，并先使用测试账号验证。

[English](README_EN.md) · [与上游的详细差异](CHANGES_FROM_UPSTREAM.md) · [上游文档](https://github.com/xpzouying/xiaohongshu-mcp)

📌 **[中文产品案例：如何让 AI Agent 的写操作更可靠](docs/PRODUCT_CASE_ZH.md)**

## 为什么做这个分支

浏览器自动化的失败并不总等于操作没有发生。比如提交按钮已经生效，但客户端在看到成功提示前超时；如果 Agent 把超时直接当失败并重试，就可能产生重复内容。

这个分支把写操作建模为可查询、可恢复的任务，并明确区分“确认失败”和“结果未知”。核心原则是：**不确定时先核实，不盲目重发。**

## 主要改动

- 图文发布、顶层评论、评论回复改为异步任务，调用后立即返回任务 ID。
- 支持显式 `idempotency_key`；未提供时，对相同请求在 10 分钟窗口内自动去重。
- 任务持久化到本地；服务重启后恢复排队任务，并把执行中断但无法确认的任务标记为 `unknown` / `uncertain`。
- 回复必须使用 `comment_id` 精确定位，不使用 `user_id` 猜测目标。
- 回复操作记录页面阶段、网络请求状态和服务端接受/拒绝证据，区分 `confirmed`、`rejected`、`blocked` 与 `uncertain`。
- 同一条评论的在途任务及近期不确定/已确认任务会触发冷却，防止换正文或换幂等键绕过去重。
- 全局浏览器槽位把浏览器操作串行化，避免多个自动化页面争抢同一账号状态。
- 推荐列表、搜索结果和用户主页默认返回精简字段与数量上限，减少 Agent 上下文占用。
- 登录二维码会话只保留一个，新请求会结束旧会话，避免后台浏览器堆积。
- MCP 与 HTTP 路由不暴露删除 Cookie 的入口，降低模型误清登录状态的风险。
- 修复发布页标题输入框因页面刷新而失效的问题；保留上游最新版的功能引导关闭逻辑。

## 写操作状态

| 操作 | 提交工具 | 查询工具 | 关键终态 |
| --- | --- | --- | --- |
| 图文发布 | `publish_content` | `get_publish_status` | `succeeded`, `failed`, `unknown` |
| 顶层评论 | `post_comment_to_feed` | `get_comment_status` | `confirmed`, `unknown` |
| 评论回复 | `reply_comment_in_feed` | `get_reply_status` | `confirmed`, `rejected`, `blocked`, `wrong_target`, `uncertain` |

`queued` 和 `running` 表示尚未结束。`unknown` / `uncertain` 表示操作可能已经到达站点，调用方应先检查实际页面，不能直接重试。

视频发布目前仍沿用上游的同步行为。

## 快速开始

要求 Go 1.24 或更高版本。完整的 Docker、客户端和登录说明可参考[上游文档](https://github.com/xpzouying/xiaohongshu-mcp)。

```bash
git clone https://github.com/mimi17-shq/xiaohongshu-mcp-reliable.git
cd xiaohongshu-mcp-reliable
go build -o xiaohongshu-mcp .
./xiaohongshu-mcp -headless=false
```

也可以从源码构建本分支的 Docker 镜像；本项目目前不发布预构建镜像，避免误把上游镜像当作可靠性分支使用：

```bash
docker compose -f docker/docker-compose.yml up --build -d
```

服务默认监听 `:18060`，MCP Streamable HTTP 端点为：

```text
http://localhost:18060/mcp
```

以 Claude Code 为例：

```bash
claude mcp add --transport http xiaohongshu-mcp http://localhost:18060/mcp
```

生产部署建议使用环境变量开启 Bearer Token 鉴权：

```bash
AUTH_TOKEN=replace-with-a-strong-random-value ./xiaohongshu-mcp
```

也支持：

- `XHS_PROXY`：浏览器代理地址。
- `XHS_FP_SEED`：固定浏览器指纹 seed。
- `XHS_PUBLISH_TASK_FILE`：发布任务状态文件路径。
- `XHS_COMMENT_TASK_FILE`：顶层评论任务状态文件路径。
- `XHS_REPLY_TASK_FILE`：回复任务状态文件路径。

默认任务文件位于用户配置目录的 `xhs-mcp/runtime/` 下。它们可能包含待执行请求内容，不应提交到版本库或公开分享。

## Agent 调用建议

1. 每个业务动作生成稳定且唯一的 `idempotency_key`。
2. 提交后保存 `task_id`，轮询对应状态工具。
3. 客户端超时后，先用原幂等键重新提交或按目标反查任务，不要构造新请求重试。
4. 遇到 `unknown` / `uncertain`，人工或通过只读工具核实站点结果。
5. 不要并发驱动同一账号；本服务会串行化浏览器访问，但调用方仍应控制队列长度。

## 开发与验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

自动化页面依赖站点 DOM，单元测试通过不代表真实账号操作一定成功。公开发布前应使用专门测试账号完成一次登录、发布、评论、回复和重启恢复验收。

## 上游与许可证

本项目派生自 [`xpzouying/xiaohongshu-mcp`](https://github.com/xpzouying/xiaohongshu-mcp)，当前整理基线为上游 `v2.5.5`（提交 `a5c8f779`）。上游版权和贡献者署名均予保留；本分支修改见 [CHANGES_FROM_UPSTREAM.md](CHANGES_FROM_UPSTREAM.md)。

代码按 [Apache License 2.0](LICENSE) 发布。

**风险提示：自动化操作可能导致账号功能受限或封禁；使用者应自行评估，并自行承担由此产生的全部风险。**
