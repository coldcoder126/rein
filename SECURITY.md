# Security Policy

## Supported versions

Only the latest v0.x release receives security fixes.

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a public issue.

You can expect a first response within 7 days. This is a best-effort project, so fixes are scheduled by severity.

## What is not a vulnerability in rein

rein runs model-driven agents, and some risks belong to that design or to the application built on it:

- **Prompt injection.** Content the model reads (web pages, files, tool results) can steer it into calling tools. Gate sensitive tools with approval in a `WrapTool` middleware.
- **`shell.Local`** runs commands directly on the host and is not a sandbox. Use an isolated `shell.Executor` in production.
- **MCP servers** are untrusted: their tool results, instructions and annotations are passed through, not verified.
- **Your own implementations** of `session.Store`, `shell.Executor`, tools and middlewares are outside the framework.

Reports showing that rein breaks its own documented guarantees (for example, a tool that was not offered to the model being executed) are in scope.
