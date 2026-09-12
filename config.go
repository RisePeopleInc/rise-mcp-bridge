package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// SharedConfig holds the per-user PROXY credentials, shared by every plugin that
// uses the bridge. Written once by the setup flow into <config-dir>/config.json
// (default ~/.rise-mcp-bridge). The target MCP endpoint and auth mode are NOT
// stored here — each consuming plugin passes those per launch (--mcp-endpoint /
// --auth), so one install + one credential prompt serves any number of plugins.
type SharedConfig struct {
	ProxyHost string `json:"proxy_host"`
	ProxyUser string `json:"proxy_user"`
	ProxyPass string `json:"proxy_pass"`
	CAFile    string `json:"ca_file,omitempty"`

	// Back-compat: single-plugin configs (≤ v0.1.x) embedded these. Used only as a
	// fallback when the matching flag isn't passed.
	MCPEndpoint string `json:"mcp_endpoint,omitempty"`
	Auth        string `json:"auth,omitempty"`
	BearerToken string `json:"bearer_token,omitempty"`
}

// defaultConfigDir is the stable, cross-OS install/config location:
// ~/.rise-mcp-bridge (uniform path so a plugin's .mcp.json works on macOS and
// Windows alike). Overridable via RISE_MCP_BRIDGE_CONFIG_DIR.
func defaultConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("RISE_MCP_BRIDGE_CONFIG_DIR")); v != "" {
		return v
	}
	// Must agree with what the consuming plugin's .mcp.json expands to. The Claude
	// desktop app only substitutes a per-OS safelist of host variables into a plugin
	// server command — HOME on macOS/Linux, USERPROFILE on Windows (never HOME) — so
	// plugins launch `${HOME:-}${USERPROFILE:-}/.rise-mcp-bridge/rise-mcp-bridge.exe`.
	// os.UserHomeDir reads exactly those variables (HOME / USERPROFILE), so install
	// and launch always resolve to the same directory. A user-set HOME on Windows is
	// deliberately ignored: the host never expands it there.
	if home := userHome(); home != "" {
		return filepath.Join(home, ".rise-mcp-bridge")
	}
	return ".rise-mcp-bridge"
}

// userHome resolves the user's home the same way the Claude host does when it
// expands ${HOME} / ${USERPROFILE}, with an OS-account lookup as a last resort for
// hosts that spawn us with a stripped environment.
func userHome() string {
	if runtime.GOOS == "windows" {
		if h := strings.TrimSpace(os.Getenv("USERPROFILE")); h != "" {
			return h
		}
	} else if h := strings.TrimSpace(os.Getenv("HOME")); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return home
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.HomeDir) != "" {
		return u.HomeDir
	}
	return ""
}

func configPath(dir string) string { return filepath.Join(dir, "config.json") }

func loadShared(dir string) (*SharedConfig, error) {
	raw, err := os.ReadFile(configPath(dir))
	if err != nil {
		return nil, err
	}
	var c SharedConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", configPath(dir), err)
	}
	return &c, nil
}

func saveShared(dir string, c *SharedConfig) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(dir), raw, 0o600)
}

func (c *SharedConfig) hasProxyCreds() bool {
	return strings.TrimSpace(c.ProxyHost) != "" && c.ProxyUser != "" && c.ProxyPass != ""
}

// proxyURL builds the https:// proxy URL from the raw credentials, percent-encoding
// the userinfo correctly (so callers store raw creds, never encoded).
func (c *SharedConfig) proxyURL() (string, error) {
	host := strings.TrimSpace(c.ProxyHost)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimRight(host, "/")
	if host == "" || c.ProxyUser == "" || c.ProxyPass == "" {
		return "", fmt.Errorf("proxy credentials missing — run the Rise MCP Bridge setup")
	}
	u := url.URL{Scheme: "https", User: url.UserPassword(c.ProxyUser, c.ProxyPass), Host: host}
	return u.String(), nil
}
