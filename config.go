package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is the per-user configuration written by a consuming plugin's setup
// skill into the bridge's config dir (typically ${CLAUDE_PLUGIN_DATA}). It is
// NEVER committed to a repo — it lives only in the user's plugin data dir.
//
// Example config.json (OAuth, the Metabase case):
//
//	{
//	  "mcp_endpoint": "https://metabase.internal.risepeople.com/api/metabase-mcp",
//	  "proxy_url":    "https://USER:PASS@rise-proxy-ca-west-1.rise.xyz:65180",
//	  "auth":         "oauth"
//	}
//
// Example (static bearer token):
//
//	{
//	  "mcp_endpoint": "https://tool.internal.risepeople.com/mcp",
//	  "proxy_url":    "https://USER:PASS@rise-proxy-ca-west-1.rise.xyz:65180",
//	  "auth":         "bearer",
//	  "bearer_token": "..."
//	}
//
// proxy_url credentials MUST be URL-encoded (@->%40, :->%3A, #->%23, %->%25);
// the consuming setup skill handles encoding before writing this file.
type Config struct {
	// MCPEndpoint is the full URL of the remote Streamable-HTTP MCP endpoint to
	// bridge to (e.g. .../api/metabase-mcp). For OAuth, this host's Site URL must
	// match what the server advertises in OAuth discovery.
	MCPEndpoint string `json:"mcp_endpoint"`

	// ProxyURL is the Rise HTTPS forward proxy with Basic-auth creds embedded.
	// Scheme MUST be https:// (TLS to the proxy itself).
	ProxyURL string `json:"proxy_url"`

	// Auth selects the upstream auth mode: "oauth" (default), "bearer", or "none".
	Auth string `json:"auth"`

	// BearerToken is used when Auth == "bearer".
	BearerToken string `json:"bearer_token,omitempty"`

	// CAFile optionally points to a PEM bundle to trust for the UPSTREAM TLS
	// connection (e.g. an internal CA fronting the MCP server). Empty = system roots.
	CAFile string `json:"ca_file,omitempty"`
}

func configDir(flagVal string) (string, error) {
	for _, c := range []string{flagVal, os.Getenv("RISE_MCP_BRIDGE_CONFIG_DIR"), os.Getenv("CLAUDE_PLUGIN_DATA")} {
		if strings.TrimSpace(c) != "" {
			return c, nil
		}
	}
	return "", fmt.Errorf("no config dir: pass --config-dir or set RISE_MCP_BRIDGE_CONFIG_DIR / CLAUDE_PLUGIN_DATA")
}

// LoadConfig reads and validates config.json from the resolved config dir.
func LoadConfig(flagVal string) (*Config, string, error) {
	dir, err := configDir(flagVal)
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, dir, fmt.Errorf("config not found at %s — run the plugin's setup skill first", path)
		}
		return nil, dir, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, dir, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Auth == "" {
		c.Auth = "oauth"
	}
	if err := c.validate(); err != nil {
		return nil, dir, err
	}
	return &c, dir, nil
}

func (c *Config) validate() error {
	mu, err := url.Parse(c.MCPEndpoint)
	if err != nil || mu.Scheme == "" || mu.Host == "" {
		return fmt.Errorf("mcp_endpoint is not a valid absolute URL: %q", c.MCPEndpoint)
	}
	pu, err := url.Parse(c.ProxyURL)
	if err != nil || pu.Host == "" {
		return fmt.Errorf("proxy_url is not a valid URL: %q", c.ProxyURL)
	}
	if pu.Scheme != "https" {
		return fmt.Errorf("proxy_url scheme must be https:// (got %q) — the Rise proxy expects a TLS connection to itself", pu.Scheme)
	}
	switch c.Auth {
	case "oauth", "none":
	case "bearer":
		if strings.TrimSpace(c.BearerToken) == "" {
			return fmt.Errorf("auth is \"bearer\" but bearer_token is empty")
		}
	default:
		return fmt.Errorf("auth must be one of oauth|bearer|none (got %q)", c.Auth)
	}
	return nil
}
