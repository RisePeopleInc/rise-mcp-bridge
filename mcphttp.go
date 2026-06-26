package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/oauth2"
)

// Pump bridges the two MCP transports:
//
//	Claude  <--stdio (newline-delimited JSON-RPC)-->  [this bridge]  <--Streamable HTTP (proxied)-->  MCP server
//
// stdin  : JSON-RPC messages from Claude, one per line.
// stdout : JSON-RPC messages back to Claude, one per line.
// Upstream: HTTP POST of each client message to <MCP endpoint>; the response is
// either application/json (one message) or text/event-stream (zero+ messages).
//
// >>> VALIDATION SPIKE (Rise eng): confirm against the live instance —
// >>>  - Mcp-Session-Id header handling on initialize,
// >>>  - whether a background GET SSE stream is needed for server-initiated
// >>>    messages (notifications/requests), and
// >>>  - protocol-version header echoing.
// >>> The request/response shapes below follow the MCP Streamable HTTP spec.
type Pump struct {
	client      *http.Client
	endpoint    string
	tokenSource oauth2.TokenSource

	out   *bufio.Writer
	outMu sync.Mutex

	sessionID string
}

func NewPump(client *http.Client, endpoint string, ts oauth2.TokenSource, out io.Writer) *Pump {
	return &Pump{
		client:      client,
		endpoint:    endpoint,
		tokenSource: ts,
		out:         bufio.NewWriter(out),
	}
}

// Run reads client messages from in until EOF.
func (p *Pump) Run(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20) // allow large messages
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(nil), line...)
		if err := p.forward(ctx, msg); err != nil {
			// Surface transport errors on stderr; do not kill the session for a
			// single failed message.
			fmt.Fprintf(stderr(), "[rise-mcp-bridge] forward error: %v\n", err)
		}
	}
	return scanner.Err()
}

func (p *Pump) forward(ctx context.Context, msg []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	// tokenSource is nil when auth == "none".
	if p.tokenSource != nil {
		tok, err := p.tokenSource.Token()
		if err != nil {
			return fmt.Errorf("get token: %w", err)
		}
		tok.SetAuthHeader(req)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if p.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", p.sessionID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		p.sessionID = sid
	}

	switch {
	case resp.StatusCode == http.StatusAccepted:
		// 202: notification/response accepted, no body to relay.
		return nil
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		return p.relaySSE(resp.Body)
	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return p.writeLine(bytes.TrimSpace(body))
	default:
		// Some servers return empty 200s; nothing to relay.
		return nil
	}
}

// relaySSE parses a text/event-stream body and writes each `data:` JSON payload
// to stdout as a newline-delimited JSON-RPC message.
func (p *Pump) relaySSE(body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil
		}
		return p.writeLine([]byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		default:
			// ignore `event:`, `id:`, comments (`:`), etc.
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return scanner.Err()
}

func (p *Pump) writeLine(b []byte) error {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	if _, err := p.out.Write(b); err != nil {
		return err
	}
	if err := p.out.WriteByte('\n'); err != nil {
		return err
	}
	return p.out.Flush()
}

// --- small helpers shared across files ---

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}
