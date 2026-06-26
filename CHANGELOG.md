# Changelog

All notable changes to rise-mcp-bridge are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.6] — 2026-06-19

### Changed

- **Setup now tells users to open a *new* chat, not restart.** Field testing showed that restarting the Claude app and returning to the same chat does **not** pick up a freshly installed bridge — a chat initializes its connectors when it opens, so only a brand-new chat spawns the connector. The success page's final step is now "Open a new chat in Claude" (was "Restart Claude or start a new chat"), with a line explaining the old chat stays disconnected even after a restart.

## [0.2.5] — 2026-06-19

### Changed

- **Removed Metabase-specific wording from the generic bridge.** The bridge is shared across any proxy-gated Rise plugin, so it no longer names Metabase anywhere user-facing: the setup page's step tracker drops the "Approve the Metabase sign-in" step (the bridge doesn't know its downstream tool — that step belongs in each consuming plugin's own setup skill) and now shows three generic steps (Install → Enter credentials → Restart). The OAuth flow's browser/tab/timeout messages and the stdio log prefix are genericized too (`[rise-mcp-bridge]`, "Opening your browser to sign in…", "Signed in."). Any per-tool sign-in step now lives only in the plugin's setup skill.

## [0.2.4] — 2026-06-19

### Added

- **Setup verifies the proxy credentials before saving.** The setup form now round-trips a probe request through the proxy with the entered username/password and only saves if they're accepted. A wrong password is caught immediately (HTTP 407) — the form re-renders inline with an error and keeps the host/username filled in so the user can fix just the password and resubmit, no app restart. Verification fails *closed* only on a definitive 407; an unreachable probe host or flaky network never blocks a correct credential (it saves with a note). After a *second* consecutive rejection the error escalates to suggest contacting IT (the account may be locked or its password changed), since by then it's likely more than a typo. Implemented as `verifyProxyCreds` in `proxy.go`.
- **Show/hide password toggle** on the setup form, so users can confirm what they typed before submitting.
- **Rise-branded setup pages with a step tracker.** The setup form and the success page now use the Rise Design System (Source Serif / Open Sans, Deep Blue + Primary purple, brand-tinted card shadow, inlined Rise wordmark) and show a 4-step progress tracker — Install the bridge → Enter credentials → Restart Claude → Approve the Metabase sign-in — so the user always sees what's done and what's left. Fonts load from Google Fonts with Georgia/Verdana fallbacks, so the page still renders cleanly offline.

## [0.2.3] — 2026-06-19

### Fixed

- **Windows: installer demanded admin rights.** The exe shipped with no embedded application manifest, so Windows fell back to installer-detection heuristics — it scans the binary, finds strings like "install"/"self-install", concludes it's an installer, and demands UAC elevation. Standard users (no admin) were blocked entirely.
  - **Fix:** embed an application manifest (`packaging/windows/app.manifest`) declaring `requestedExecutionLevel = asInvoker`, wired in through `goversioninfo` via `versioninfo.json` `ManifestPath`. An explicit execution level disables installer detection, so the bridge runs as the launching user with no elevation. It only ever writes to `%USERPROFILE%\.rise-mcp-bridge`, so it never needs admin.

### Notes

- The Windows SmartScreen "Windows protected your PC" prompt is **reputation-based**, not a signing failure — a newly published signed binary trips it until download reputation accrues. Users click **More info → Run anyway**; this needs no admin rights. (Documented in the `rise-metabase` `metabase-setup` skill.)
- **CI guard:** the Windows build now asserts (on every build, including dry runs) that the exe carries an embedded `asInvoker` RT_MANIFEST and the version/metadata resource (`mt.exe` + `FileVersionInfo`), so a silent `goversioninfo` embedding miss fails the build instead of resurfacing the admin prompt for a user.

## [0.2.2] — 2026-06-19

### Fixed

- **macOS: installed bridge was killed at launch ("invalid Info.plist").** The installer self-installed the `.app`'s *main* executable (`Contents/MacOS/RiseMCPBridge`), which is signed in bundle context — its signature is bound to the bundle `Info.plist`, so the copy at `~/.rise-mcp-bridge/rise-mcp-bridge` failed signature validation and AMFI killed it with SIGKILL before any code ran. The symptom in Cowork was the `rise-metabase` connector stuck "connecting" then failing, with no OAuth tab and no logs (the process never executed). The `.app` itself was unaffected because it launches as a bundle with its `Info.plist` + stapled ticket present.
  - **Fix:** the `.app` now ships a second, **standalone-signed** copy of the binary (`Contents/MacOS/rise-mcp-bridge`, a loose Mach-O whose signature has no `Info.plist` binding), and `selfInstall` installs *that* instead of the bundle main exe. A CI guard copies the payload out of the bundle and re-runs `codesign --verify --strict` on the bare file, so this class of bug fails the release build instead of shipping. `selfInstall` also strips `com.apple.quarantine` from the installed copy as belt-and-suspenders.

## [0.2.1] — 2026-06-19

### Added

- **App icon (both platforms).** The downloaded installer now carries the Rise icon instead of a generic placeholder, so it's recognizably Rise's and doesn't look suspicious.
  - **macOS** (`RiseMCPBridge.app`): ships `packaging/macos/AppIcon.icns`, wired in via `CFBundleIconFile`. Bundled into `Contents/Resources` before codesigning, so it's covered by the signature.
  - **Windows** (`RiseMCPBridge.exe`): a Win32 resource (`packaging/windows/icon.ico`, derived from the same artwork) is compiled into the binary via `goversioninfo` (`resource_windows_amd64.syso`) before the build, so it's covered by the Azure signature. The same resource embeds publisher/version metadata (CompanyName, ProductName, FileDescription, version) so the file's Properties → Details tab is populated rather than blank.

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
