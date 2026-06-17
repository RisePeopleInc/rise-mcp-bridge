# rise-mcp-bridge

A generic, self-contained **MCP stdio bridge** that connects an MCP client
(Claude in Cowork / Claude Code) to any remote **Streamable-HTTP MCP server**,
routing all traffic through the **Rise HTTPS proxy** so requests reach the server
from the allowlisted proxy egress IP.

```
MCP client ── stdio (JSON-RPC) ──► rise-mcp-bridge ── Streamable HTTP (via Rise proxy) ──► MCP server
```

One static binary, **no Node/npx**. Built for reuse: any internal tool that
exposes a Streamable-HTTP MCP endpoint behind the Rise proxy can be reached
through it. The first consumer is the `rise-metabase` plugin (Metabase v0.60+
`/api/metabase-mcp`).

> Seeded here under the `rise-plugins` workspace for review. It is intended to be
> extracted into its own GitHub repo (`RisePeopleInc/rise-mcp-bridge`) so signing
> secrets and release artifacts live outside the marketplace repo.

## Why a Go binary

The Rise proxy is a **secure web proxy** — the client must speak TLS *to the
proxy itself* (TLS-in-TLS). Go's `http.Transport` supports `https://` proxies
natively (`proxy.go`), sidestepping the TLS-in-TLS limitations of many Node HTTP
clients, and ships as a single dependency-free binary.

## Configuration

Per-user `config.json` in the config dir (`${CLAUDE_PLUGIN_DATA}`), written by the
consuming plugin's setup skill — never committed:

| field | meaning |
|---|---|
| `mcp_endpoint` | full URL of the remote Streamable-HTTP MCP endpoint |
| `proxy_url` | `https://USER:PASS@host:port` (URL-encoded creds; https only) |
| `auth` | `oauth` (default) · `bearer` · `none` |
| `bearer_token` | used when `auth: bearer` |
| `ca_file` | optional PEM bundle for upstream TLS (internal CA) |

## Releases & distribution

CI (`.github/workflows/release.yml`) builds on a `v*` tag and publishes to a
GitHub Release:

- `rise-mcp-bridge-darwin-universal` — codesigned + **notarized** (required; an
  unsigned mac binary is Gatekeeper-blocked)
- `rise-mcp-bridge-windows-amd64.exe` — sign with your Windows cert before upload
- `rise-mcp-bridge-linux-amd64`
- `SHA256SUMS`

Consuming plugins **fetch the pinned release at setup time and verify against
`SHA256SUMS`** — binaries are not committed to the marketplace repo.

Required CI secrets: `MACOS_CERT_P12_BASE64`, `MACOS_CERT_PASSWORD`,
`KEYCHAIN_PASSWORD`, `DEVELOPER_ID_APP`, `APPLE_ID`, `APPLE_TEAM_ID`,
`APPLE_APP_PASSWORD`.

**Build prerequisite:** run `go mod tidy` once to generate and commit `go.sum`
(this sandbox had no Go toolchain, so it isn't generated yet).

## Validation spike (before > v0.1.0)

`proxy.go` (the proxy tunnel) is the confident, load-bearing part. These need a
live check and may need adjustment — see inline `VALIDATE` / `TODO(validate)`:

1. OAuth discovery via `WWW-Authenticate: resource_metadata` (`oauth.go`).
2. Dynamic client registration request/response shape (`oauth.go`).
3. Streamable-HTTP session handling: `Mcp-Session-Id`, background GET SSE stream,
   protocol-version header (`mcphttp.go`).
