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
// Preferred schema — RAW proxy credentials; the bridge does the URL-encoding, so
// a setup skill can write this file with no encoding logic of its own:
//
//	{
//	  "mcp_endpoint": "https://metabase.example.com/api/metabase-mcp",
//	  "proxy_host":   "proxy.example.com:PORT",
//	  "proxy_user":   "USER",
//	  "proxy_pass":   "PASS",
//	  "auth":         "oauth"
//	}
//
// Back-compat: a pre-built "proxy_url" (with URL-encoded credentials) is still
// accepted in place of the proxy_host/proxy_user/proxy_pass trio.
type Config struct {
	// MCPEndpoint is the full URL of the remote Streamable-HTTP MCP endpoint to
	// bridge to (e.g. .../api/metabase-mcp). For OAuth, this host's Site URL must
	// match what the server advertises in OAuth discovery.
	MCPEndpoint string `json:"mcp_endpoint"`

	// Preferred: raw proxy credentials. The bridge percent-encodes them when it
	// builds the proxy URL, so callers write them verbatim (no client-side encoding).
	ProxyHost string `json:"proxy_host,omitempty"`
	ProxyUser string `json:"proxy_user,omitempty"`
	ProxyPass string `json:"proxy_pass,omitempty"`

	// Back-compat: a pre-built https:// proxy URL with Basic-auth creds embedded
	// (URL-encoded). Used as-is when set, taking precedence over the trio above.
	ProxyURL string `json:"proxy_url,omitempty"`

	// Auth selects the upstream auth mode: "oauth" (default), "bearer", or "none".
	Auth string `json:"auth"`

	// BearerToken is used when Auth == "bearer".
	BearerToken string `json:"bearer_token,omitempty"`

	// CAFile optionally points to a PEM bundle to trust for the UPSTREAM TLS
	// connection (e.g. an internal CA fronting the MCP server). Empty = system roots.
	CAFile string `json:"ca_file,omitempty"`
}

// proxyURL returns the effective https:// proxy URL with credentials. If
// proxy_url is set it's used verbatim (back-compat); otherwise it's built from
// the raw proxy_host/proxy_user/proxy_pass with correct percent-encoding applied
// here — so consumers can write raw credentials into config.json.
func (c *Config) proxyURL() (string, error) {
	if strings.TrimSpace(c.ProxyURL) != "" {
		return c.ProxyURL, nil
	}
	host := strings.TrimSpace(c.ProxyHost)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimRight(host, "/")
	if host == "" || c.ProxyUser == "" || c.ProxyPass == "" {
		return "", fmt.Errorf("proxy config incomplete: set proxy_host + proxy_user + proxy_pass (or a pre-encoded proxy_url)")
	}
	// url.UserPassword + URL.String() percent-encode the userinfo correctly.
	u := url.URL{Scheme: "https", User: url.UserPassword(c.ProxyUser, c.ProxyPass), Host: host}
	return u.String(), nil
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
	pURL, err := c.proxyURL()
	if err != nil {
		return err
	}
	pu, err := url.Parse(pURL)
	if err != nil || pu.Host == "" {
		return fmt.Errorf("could not resolve a valid proxy URL from the config")
	}
	if pu.Scheme != "https" {
		return fmt.Errorf("proxy must be https:// — the Rise proxy expects a TLS connection to itself")
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
