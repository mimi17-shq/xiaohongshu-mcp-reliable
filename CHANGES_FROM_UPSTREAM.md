# Changes from upstream

This repository is a derivative of
[`xpzouying/xiaohongshu-mcp`](https://github.com/xpzouying/xiaohongshu-mcp),
licensed under Apache-2.0.

Preparation baseline: upstream tag `v2.5.5`, commit `a5c8f7799980ba1fdd501999843eb2d17e4c9a9f`.

## Reliability changes

- Added durable asynchronous task managers for image-post publishing, top-level comments, and comment replies.
- Added caller-provided idempotency keys and fallback request-hash deduplication windows.
- Added restart recovery for queued tasks and conservative `unknown` / `uncertain` outcomes for interrupted side effects.
- Added task status tools and target-based lookup for comments and replies.
- Added a global browser-session gate and context-aware acquisition.
- Added single-active-session handling for QR-code login.

## Reply safety changes

- Require `comment_id` for deterministic reply targeting.
- Verify the target comment and track reply progress through page and network phases.
- Record whether a correctly targeted request was allowed, whether the server explicitly accepted or rejected it, and whether the new reply was observed.
- Block concurrent and recent duplicate operations for the same `feed_id + comment_id` target.
- Treat ambiguous browser failures as non-retryable uncertainty rather than definite failure.

## Response and browser fixes

- Return bounded, slim feed/search/profile payloads for lower MCP context usage.
- Improve notification parsing and preserve unknown comment levels instead of guessing.
- Reacquire the post-title element after dynamic page updates to avoid stale-element errors.
- Keep the upstream feature-guide dismissal added in `v2.5.5`.

## Reduced destructive surface

- The upstream Cookie deletion implementation remains available internally, but no Cookie deletion route or MCP tool is registered in this fork.

## Independent distribution cleanup

- Removed upstream maintainer instructions, donation artifacts, publishing skills, funding/ownership metadata, and release workflows that were tied to the upstream maintainer's accounts.
- Docker Compose builds this fork from source instead of pulling the upstream image.
- API repository metadata points to this fork. The Go module path remains unchanged for source compatibility with the upstream package layout.

## Tests

- Added task persistence, deduplication, recovery, state-transition, browser-gate, reply-targeting, schema, response-slimming, and verification tests.
- Extended upstream notification and publishing tests for the modified behavior.

Files changed from upstream carry a modification notice. The original Apache-2.0 license and upstream attribution are retained.
