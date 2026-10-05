# Xiaohongshu MCP Reliable

A reliability-focused Xiaohongshu/RedNote MCP fork for AI agents, based on
[`xpzouying/xiaohongshu-mcp`](https://github.com/xpzouying/xiaohongshu-mcp).

The upstream project already provides login, browsing, search, publishing, comments, likes, favorites, and notifications. This fork focuses on a different problem: preventing duplicate posts, duplicate comments, and replies to the wrong thread when browser automation times out, the service restarts, or an agent repeats a call.

> This is an unofficial community fork and is not affiliated with Xiaohongshu or the upstream maintainers. Browser automation can break when the site changes. Follow applicable terms and laws, and validate the workflow with a test account first.

[中文](README.md) · [Detailed upstream differences](CHANGES_FROM_UPSTREAM.md) · [Upstream documentation](https://github.com/xpzouying/xiaohongshu-mcp)

## Why this fork exists

A browser automation error does not always mean that no side effect occurred. A submit click may already have reached the site before the client times out. Treating that timeout as a safe failure can make an agent post the same content twice.

This fork models write operations as durable, queryable tasks and distinguishes a confirmed failure from an unknown outcome. Its core rule is: **verify uncertain outcomes; never retry them blindly.**

## Highlights

- Image-post publishing, top-level comments, and comment replies run as asynchronous tasks and immediately return a task ID.
- Explicit `idempotency_key` support, plus a 10-minute request-hash deduplication window when no key is supplied.
- Local task persistence. Queued jobs resume after restart; interrupted in-flight jobs become `unknown` / `uncertain` instead of falsely retryable failures.
- Replies require an exact `comment_id`; `user_id` is never used to guess the target.
- Reply tasks record page phases, network-request state, and server acceptance/rejection evidence.
- Per-target cooldown blocks duplicate replies even if the text or idempotency key changes.
- A global browser slot serializes browser sessions to avoid concurrent state conflicts on one account.
- Slimmed, bounded feed/search/profile responses reduce agent context usage.
- Only one QR-login session is kept alive; requesting a new QR cancels the previous session.
- Cookie deletion is not exposed through MCP or HTTP routes, reducing accidental logout by an agent.
- The publish-title stale-element failure is handled while retaining the latest upstream feature-guide behavior.

## Write-operation states

| Operation | Submit tool | Status tool | Important terminal states |
| --- | --- | --- | --- |
| Image post | `publish_content` | `get_publish_status` | `succeeded`, `failed`, `unknown` |
| Top-level comment | `post_comment_to_feed` | `get_comment_status` | `confirmed`, `unknown` |
| Comment reply | `reply_comment_in_feed` | `get_reply_status` | `confirmed`, `rejected`, `blocked`, `wrong_target`, `uncertain` |

`queued` and `running` are non-terminal. `unknown` / `uncertain` means that the request may already have reached the site; inspect the actual page before retrying.

Video publishing currently keeps the upstream synchronous behavior.

## Quick start

Go 1.24 or newer is required. See the [upstream documentation](https://github.com/xpzouying/xiaohongshu-mcp) for complete Docker, client, and login instructions.

```bash
git clone <your-fork-url>
cd xiaohongshu-mcp-reliable
go build -o xiaohongshu-mcp .
./xiaohongshu-mcp -headless=false
```

The service listens on `:18060` by default. Its Streamable HTTP MCP endpoint is:

```text
http://localhost:18060/mcp
```

Claude Code example:

```bash
claude mcp add --transport http xiaohongshu-mcp http://localhost:18060/mcp
```

Enable Bearer-token authentication in production:

```bash
AUTH_TOKEN=replace-with-a-strong-random-value ./xiaohongshu-mcp
```

Supported environment variables also include:

- `XHS_PROXY`: browser proxy URL.
- `XHS_FP_SEED`: stable browser fingerprint seed.
- `XHS_PUBLISH_TASK_FILE`: publish-task state file.
- `XHS_COMMENT_TASK_FILE`: top-level-comment task state file.
- `XHS_REPLY_TASK_FILE`: reply-task state file.

Task files default to `xhs-mcp/runtime/` under the user configuration directory. They can contain pending request content and must not be committed or shared publicly.

## Agent integration guidance

1. Generate one stable, unique `idempotency_key` per business action.
2. Save the returned `task_id` and poll the matching status tool.
3. After a client timeout, retry with the same idempotency key or look up the existing task; do not construct a new request.
4. For `unknown` / `uncertain`, inspect the site using read-only tools or manually.
5. Avoid concurrent automation against one account. The service serializes browser access, but callers should still bound their queues.

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
```

Passing unit tests cannot prove that live DOM automation works after a site update. Before a public release, validate login, publishing, commenting, replies, and restart recovery with a dedicated test account.

## Upstream and license

This project is derived from [`xpzouying/xiaohongshu-mcp`](https://github.com/xpzouying/xiaohongshu-mcp). The current preparation baseline is upstream `v2.5.5` (commit `a5c8f779`). Upstream copyright and contributor attribution are retained. See [CHANGES_FROM_UPSTREAM.md](CHANGES_FROM_UPSTREAM.md) for the fork's modifications.

Licensed under the [Apache License 2.0](LICENSE).

**Risk notice: automation may result in account restrictions or bans. You are solely responsible for evaluating and accepting all risks arising from its use.**
