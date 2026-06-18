// Command rise-mcp-bridge is a self-contained, single-binary MCP stdio bridge
// that connects an MCP client (Claude in Cowork / Claude Code) to a remote
// Streamable-HTTP MCP server, routing ALL traffic through the Rise HTTPS proxy
// so the request reaches the server from the allowlisted proxy IP.
//
// It is generic: any internal tool that exposes a Streamable-HTTP MCP endpoint
// behind the Rise proxy can be reached through it. Per-user config (endpoint +
// proxy creds + auth mode) is read from the config dir (typically
// ${CLAUDE_PLUGIN_DATA}), written by the consuming plugin's setup skill.
//
// No Node/npx dependency. See README.md, including the Validation Spike list.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/oauth2"
)

const version = "0.1.0"

func stderr() *os.File { return os.Stderr }

func main() {
	configDirFlag := flag.String("config-dir", "", "directory holding config.json + cached tokens (defaults to RISE_MCP_BRIDGE_CONFIG_DIR / CLAUDE_PLUGIN_DATA)")
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println("rise-mcp-bridge " + version)
		return
	}

	if err := run(*configDirFlag); err != nil {
		fmt.Fprintf(os.Stderr, "[rise-mcp-bridge] fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(configDirFlag string) error {
	cfg, dir, err := LoadConfig(configDirFlag)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A trapped signal cancels ctx; force a prompt exit even while blocked on a
	// stdin read (otherwise Ctrl-C is swallowed during interactive runs).
	go func() { <-ctx.Done(); os.Exit(0) }()

	client, err := newProxiedClient(cfg.ProxyURL, cfg.CAFile)
	if err != nil {
		return err
	}

	var ts oauth2.TokenSource
	switch cfg.Auth {
	case "oauth":
		ts, err = Authenticate(ctx, client, cfg, tokenStore{dir: dir})
		if err != nil {
			return fmt.Errorf("authentication: %w", err)
		}
	case "bearer":
		ts = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.BearerToken, TokenType: "Bearer"})
	case "none":
		ts = nil // pump sends no Authorization header
	}

	pump := NewPump(client, cfg.MCPEndpoint, ts, os.Stdout)
	fmt.Fprintf(os.Stderr, "[rise-mcp-bridge] connected to %s via proxy (auth=%s); bridging stdio\n", cfg.MCPEndpoint, cfg.Auth)
	return pump.Run(ctx, os.Stdin)
}
