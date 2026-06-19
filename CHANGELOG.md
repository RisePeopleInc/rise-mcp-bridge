# Changelog

All notable changes to rise-mcp-bridge are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] — 2026-06-19

Reworks the bridge into a **shared, self-installing dependency** so non-technical Cowork users can set it up with no terminal, and so multiple plugins can reuse one install.

### Changed

- **Shared install location.** The bridge now lives at a stable, cross-OS path (`~/.rise-mcp-bridge`) instead of a per-plugin data dir, so any number of plugins can reuse one install. A plugin's `.mcp.json` launches `${HOME}/.rise-mcp-bridge/rise-mcp-bridge` with its own `--mcp-endpoint`/`--auth`.
- **Config split.** `config.json` now holds only the shared **proxy credentials** (`proxy_host`/`proxy_user`/`proxy_pass`, raw — the bridge percent-encodes them). The target MCP endpoint + auth mode are passed per launch, not stored. OAuth tokens + dynamic client registrations are cached **per endpoint**.

### Added

- **Self-setup browser form.** Run with `--setup` (or by double-clicking the installer app), the bridge serves a local browser form to collect the proxy username/password, writes `config.json`, and **self-installs** the binary into `~/.rise-mcp-bridge` — no terminal, no agent file-write. Reuses the same loopback machinery as the OAuth flow.
- **Installer app artifacts.** Releases now publish a notarized, **stapled** `RiseMCPBridge.app.zip` (macOS) and `RiseMCPBridge.exe` (Windows) so setup is a download + double-click. The bare `rise-mcp-bridge-*` binaries remain for Claude Code CLI / power users.

  - Notes: launch mode is decided by `--setup` / running inside a `.app` bundle / an interactive stdin → **setup**; `--mcp-endpoint` present → **server**; piped stdin with no endpoint (a stray/misconfigured MCP registration) → **errors** instead of surprising the user with a setup window.

First release. A self-contained, single-binary MCP **stdio bridge** that connects an MCP client (Claude in Cowork / Claude Code) to a remote **Streamable-HTTP MCP server**, routing all traffic through the **Rise HTTPS proxy** so requests reach the server from the allowlisted egress IP. One static binary — no Node/npx. Validated end-to-end against a live Metabase v0.62 (`/api/metabase-mcp`); the first consumer is the `rise-metabase` connector plugin, which fetches the signed binary at setup and verifies it against `SHA256SUMS`.

### Added

- **Proxied MCP stdio↔Streamable-HTTP bridge.** Speaks TLS *to* the Rise HTTPS forward proxy (TLS-in-TLS) with Basic auth, so the upstream MCP server sees the allowlisted proxy IP and nothing reaches it directly. Relays newline-delimited JSON-RPC over stdio to the remote endpoint, including SSE responses and `Mcp-Session-Id` session handling. Optional `ca_file` adds an internal CA to the upstream TLS trust.
- **OAuth 2.0 client.** Discovery (RFC 8414), dynamic client registration (RFC 7591), PKCE, and a fixed-port loopback redirect. Per-user auth, scoped to the user's Metabase permissions; tokens cached and refreshed in the config dir. Requests the agent scopes advertised in the server's `scopes_supported` — **required**, or the server filters `tools/list` to empty (see `defaultAgentScopes` in `oauth.go`).
- **Pluggable auth modes:** `oauth` (default), `bearer`, and `none`, selected in per-user `config.json` (`mcp_endpoint`, `proxy_url`, `auth`).
- **Signed, multi-platform releases.** CI builds on a `v*` tag and publishes `rise-mcp-bridge-darwin-universal` (Developer ID signed + notarized), `rise-mcp-bridge-windows-amd64.exe` (Azure Artifact Signing over GitHub OIDC), `rise-mcp-bridge-linux-amd64`, and `SHA256SUMS`. Signing reuses Rise's existing Apple Developer ID cert and Azure Artifact Signing account/profile (see `README.md` → Signing setup). A `workflow_dispatch` `skip_signing` input produces unsigned builds for dry runs.

  - Notes: macOS bare executables can't be stapled, so Gatekeeper verifies notarization online on first run. Windows SmartScreen may show a reputation prompt for a brand-new signed binary until download reputation accrues (separate from the publisher check). Generic by design — any Streamable-HTTP MCP endpoint behind the Rise proxy can be reached through it.
