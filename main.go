// Command rise-mcp-bridge is a self-contained, single-binary MCP stdio bridge
// that connects an MCP client (Claude in Cowork / Claude Code) to a remote
// Streamable-HTTP MCP server, routing all traffic through the Rise HTTPS proxy
// so the request reaches the server from the allowlisted proxy IP.
//
// Two modes:
//   - SETUP (--setup, or no --mcp-endpoint, e.g. when double-clicked): installs the
//     binary into the stable config dir (~/.rise-mcp-bridge) and collects the shared
//     proxy credentials via a local browser form. No terminal needed.
//   - SERVER (--mcp-endpoint <url>, how a plugin's .mcp.json launches it): bridges
//     stdio to that endpoint through the proxy, using the shared proxy creds + auth.
//
// One install + one credential prompt is reused by any number of plugins; each
// passes its own --mcp-endpoint. No Node/npx dependency.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/oauth2"
)

const version = "0.2.2"

func stderr() *os.File { return os.Stderr }

func main() {
	configDir := flag.String("config-dir", defaultConfigDir(), "config/install dir (default ~/.rise-mcp-bridge)")
	mcpEndpoint := flag.String("mcp-endpoint", "", "target Streamable-HTTP MCP endpoint (set by the consuming plugin)")
	authMode := flag.String("auth", "", "auth mode: oauth (default) | bearer | none")
	bearerTok := flag.String("bearer-token", "", "token when --auth=bearer")
	caFile := flag.String("ca-file", "", "optional PEM bundle to trust for the upstream TLS")
	setup := flag.Bool("setup", false, "run interactive setup (install + collect proxy credentials)")
	showVer := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVer {
		fmt.Println("rise-mcp-bridge " + version)
		return
	}

	switch {
	case *setup, launchedFromAppBundle():
		// Explicit --setup, or launched by double-clicking the installer .app.
		runOrDie(runSetup(*configDir), "setup")
	case *mcpEndpoint != "":
		// Server mode — how a plugin's .mcp.json launches it.
		runOrDie(runServer(*configDir, *mcpEndpoint, *authMode, *bearerTok, *caFile), "fatal")
	case stdinIsInteractive():
		// No endpoint and a real TTY (double-clicked / run by hand) → interactive setup.
		runOrDie(runSetup(*configDir), "setup")
	default:
		// No endpoint and stdin is a pipe → launched as an MCP server without a
		// target (e.g. a stale/misconfigured registration). Error instead of
		// surprising the user with a setup window.
		fmt.Fprintln(os.Stderr, "[rise-mcp-bridge] no --mcp-endpoint provided. A consuming plugin must pass --mcp-endpoint <url>; run with --setup to (re)configure proxy credentials.")
		os.Exit(2)
	}
}

func runOrDie(err error, label string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rise-mcp-bridge] %s failed: %v\n", label, err)
		os.Exit(1)
	}
}

// stdinIsInteractive reports whether stdin is a terminal/console (interactive run
// or a double-clicked app, where Finder/Explorer give a char device) rather than a
// pipe (how an MCP client drives a server).
func stdinIsInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// launchedFromAppBundle reports whether we're running as the executable inside a
// macOS .app bundle (the double-clicked installer), which deterministically means
// setup mode regardless of how stdin looks.
func launchedFromAppBundle() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(exe, ".app/Contents/MacOS/")
}

func runServer(configDir, mcpEndpoint, authMode, bearerTok, caFile string) error {
	shared, err := loadShared(configDir)
	if err != nil || !shared.hasProxyCreds() {
		return fmt.Errorf("proxy credentials not set in %s — run the Rise MCP Bridge setup (double-click the app) first", configDir)
	}
	if authMode == "" {
		authMode = "oauth"
		if shared.Auth != "" {
			authMode = shared.Auth
		}
	}
	if caFile == "" {
		caFile = shared.CAFile
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A trapped signal cancels ctx; force a prompt exit even while blocked on a
	// stdin read (otherwise Ctrl-C is swallowed during interactive runs).
	go func() { <-ctx.Done(); os.Exit(0) }()

	pURL, err := shared.proxyURL()
	if err != nil {
		return err
	}
	client, err := newProxiedClient(pURL, caFile)
	if err != nil {
		return err
	}

	var ts oauth2.TokenSource
	switch authMode {
	case "oauth":
		ts, err = Authenticate(ctx, client, mcpEndpoint, tokenStore{dir: configDir, key: endpointKey(mcpEndpoint)})
		if err != nil {
			return fmt.Errorf("authentication: %w", err)
		}
	case "bearer":
		tok := bearerTok
		if tok == "" {
			tok = shared.BearerToken
		}
		if tok == "" {
			return fmt.Errorf("auth=bearer but no --bearer-token provided")
		}
		ts = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"})
	case "none":
		ts = nil // pump sends no Authorization header
	default:
		return fmt.Errorf("auth must be oauth|bearer|none (got %q)", authMode)
	}

	pump := NewPump(client, mcpEndpoint, ts, os.Stdout)
	fmt.Fprintf(os.Stderr, "[rise-mcp-bridge] connected to %s via proxy (auth=%s); bridging stdio\n", mcpEndpoint, authMode)
	return pump.Run(ctx, os.Stdin)
}
