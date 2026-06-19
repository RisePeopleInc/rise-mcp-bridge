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

- `rise-mcp-bridge-darwin-universal` — Developer ID signed + **notarized**
- `rise-mcp-bridge-windows-amd64.exe` — Azure Artifact Signing (Trusted Signing)
- `rise-mcp-bridge-linux-amd64`
- `SHA256SUMS`

Consuming plugins **fetch the pinned release at setup time and verify against
`SHA256SUMS`** — binaries are not committed to the marketplace repo.

### Signing setup (reuses Rise's existing infrastructure)

This workflow reuses the same certs and Azure signing account as `raise-editor`
(see its `docs/azure-signing-setup.md`), so there's no new Apple cert or Azure
identity validation to provision.

**macOS** — copy these repo secrets from `rise-md-editor` (or promote them to
org-level): `MAC_CSC_LINK`, `MAC_CSC_KEY_PASSWORD`, `APPLE_ID`, `APPLE_TEAM_ID`,
`APPLE_APP_SPECIFIC_PASSWORD`. (We don't need `MAC_INSTALLER_CSC_*` — that's only
for `.pkg`; this ships a bare binary.) The signing identity is auto-discovered
from the imported cert.

**Windows (Azure Artifact Signing over OIDC)** — one-time wiring:

1. Add a **federated credential** to the existing Entra app
   `SIGNING-APP`: scenario *GitHub Actions*, entity *Environment*,
   repo `RisePeopleInc/rise-mcp-bridge`, environment `rise-mcp-bridge-signing`
   (subject `repo:RisePeopleInc/rise-mcp-bridge:environment:rise-mcp-bridge-signing`).
2. Create a GitHub **environment** named `rise-mcp-bridge-signing` on this repo.
3. Set repo secrets `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_SUBSCRIPTION_ID`
   (same public-identifier values as `rise-md-editor`).

The account/profile names (`SIGNING-ACCOUNT` / `CERT-PROFILE`)
are non-secret config, hardcoded in `release.yml`.

To build unsigned for a dry run: `workflow_dispatch` with `skip_signing: true`
(no secrets needed).

## Validation status

Validated end-to-end against a live Metabase (v0.62, `/api/metabase-mcp`) on
2026-06-18: proxy tunnel, OAuth (discovery + dynamic registration + PKCE +
**agent scopes**), Streamable-HTTP session handling, and a populated `tools/list`
all confirmed. Note: the OAuth authorize request **must** include the agent
scopes (`scopes_supported` from discovery) or the server filters `tools/list` to
empty — see `defaultAgentScopes` in `oauth.go`.
