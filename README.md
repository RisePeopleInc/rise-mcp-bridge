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

## How a plugin launches it

A consuming plugin's `.mcp.json` runs the installed bridge from the user's home
directory and passes its own endpoint:

```json
{
  "mcpServers": {
    "my-tool": {
      "command": "${HOME:-}${USERPROFILE:-}/.rise-mcp-bridge/rise-mcp-bridge.exe",
      "args": ["--mcp-endpoint", "https://tool.internal.example/mcp", "--auth", "oauth"]
    }
  }
}
```

The odd-looking command is deliberate. The Claude **desktop app** expands only a
per-OS safelist of host variables in plugin server commands — `HOME` on
macOS/Linux, `USERPROFILE` on Windows (never `HOME`, even if set) — and leaves any
other `${VAR}` as literal text. The `${VAR:-default}` form blanks a variable that
is off the safelist, so exactly one of the two expands on each OS. The binary is
installed under the uniform name `rise-mcp-bridge.exe` on every OS (macOS runs a
Mach-O regardless of extension) so one command string works everywhere.
`${CLAUDE_PLUGIN_DATA}` is *not* expanded by the desktop host and cannot be used.

## Configuration

Per-user `config.json` in the config dir (`~/.rise-mcp-bridge`; override with
`RISE_MCP_BRIDGE_CONFIG_DIR` or `--config-dir`), written by the bridge's own
setup flow (double-click the installer app) — never committed:

| field | meaning |
|---|---|
| `proxy_host` | Rise HTTPS proxy `host:port` (pre-filled in the setup form) |
| `proxy_user` / `proxy_pass` | the user's proxy credentials, stored raw (the bridge percent-encodes them) |
| `ca_file` | optional PEM bundle for upstream TLS (internal CA) |

The target MCP endpoint and auth mode are **not** stored: each consuming plugin
passes `--mcp-endpoint` and `--auth` (`oauth` default · `bearer` · `none`) per
launch, so one install and one credential prompt serve any number of plugins.
OAuth tokens and dynamic client registrations are cached per endpoint in the same
dir.

## Releases & distribution

CI (`.github/workflows/release.yml`) builds on a `v*` tag and publishes to a
GitHub Release:

- `rise-mcp-bridge-darwin-universal` — Developer ID signed + **notarized**
- `rise-mcp-bridge-windows-amd64.exe` — Azure Artifact Signing (Trusted Signing)
- `rise-mcp-bridge-linux-amd64`
- `SHA256SUMS`

Consuming plugins **fetch the pinned release at setup time and verify against
`SHA256SUMS`** — binaries are not committed to the marketplace repo.

### Signing setup

Maintainers: the internal provisioning runbook (Apple Developer ID + Azure
Artifact Signing account, identity validation, Entra app) lives in the private
ops docs. All instance-specific values are supplied at build time via repo
secrets and variables — nothing internal is hardcoded in this repo.

**macOS** — repo **secrets**: `MAC_CSC_LINK` (base64 Developer ID Application
`.p12`), `MAC_CSC_KEY_PASSWORD`, `APPLE_ID`, `APPLE_TEAM_ID`,
`APPLE_APP_SPECIFIC_PASSWORD`. The signing identity is auto-discovered from the
imported cert.

**Windows (Azure Artifact Signing over OIDC)** — one-time wiring:

1. Add a **federated credential** to your org's signing Entra app: scenario
   *GitHub Actions*, entity *Environment*, repo `RisePeopleInc/rise-mcp-bridge`,
   environment `rise-mcp-bridge-signing` (subject
   `repo:RisePeopleInc/rise-mcp-bridge:environment:rise-mcp-bridge-signing`).
2. Create a GitHub **environment** named `rise-mcp-bridge-signing` on this repo.
3. Repo **secrets**: `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_SUBSCRIPTION_ID`
   (public OIDC identifiers).
4. Repo **variables**: `TRUSTED_SIGNING_ENDPOINT`, `TRUSTED_SIGNING_ACCOUNT`,
   `TRUSTED_SIGNING_PROFILE` (the signing account's endpoint / account / cert
   profile names).

To build unsigned for a dry run: `workflow_dispatch` with `skip_signing: true`
(no secrets needed).

## Validation status

Validated end-to-end against a live Metabase (v0.62, `/api/metabase-mcp`) on
2026-06-18: proxy tunnel, OAuth (discovery + dynamic registration + PKCE +
**agent scopes**), Streamable-HTTP session handling, and a populated `tools/list`
all confirmed. Note: the OAuth authorize request **must** include the agent
scopes (`scopes_supported` from discovery) or the server filters `tools/list` to
empty — see `defaultAgentScopes` in `oauth.go`.
