package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	initMsg   []byte // the client's initialize request, replayed to open a new session
	needInit  bool   // the session is gone (expired, or initialize failed): open one first

	// pending holds the ids of the requests in the message being forwarded that
	// have not been answered yet; relayed responses remove theirs.
	pending map[string]bool
}

// tokenInvalidator is implemented by token sources that can be told the server
// rejected a token (sharedTokenSource).
type tokenInvalidator interface {
	invalidate(*oauth2.Token)
}

// rpcHead is the part of a JSON-RPC message the bridge needs to route it.
type rpcHead struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func (h rpcHead) isRequest() bool  { return h.Method != "" && h.hasID() }
func (h rpcHead) isResponse() bool { return h.Method == "" && h.hasID() }
func (h rpcHead) hasID() bool      { return len(h.ID) > 0 && string(h.ID) != "null" }

// parseHeads decodes a JSON-RPC message or batch (an array of messages).
func parseHeads(msg []byte) []rpcHead {
	if trimmed := bytes.TrimSpace(msg); len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []rpcHead
		_ = json.Unmarshal(trimmed, &batch)
		return batch
	}
	var head rpcHead
	if json.Unmarshal(msg, &head) != nil {
		return nil
	}
	return []rpcHead{head}
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
		p.pending = map[string]bool{}
		var requests []rpcHead
		for _, h := range parseHeads(msg) {
			if h.isRequest() {
				requests = append(requests, h)
				p.pending[string(h.ID)] = true
			}
		}
		isInit := len(requests) == 1 && requests[0].Method == "initialize"
		if isInit {
			p.initMsg = msg
		}
		err := p.forward(ctx, msg, isInit)
		if isInit {
			p.needInit = err != nil
		}
		if err != nil {
			// Surface transport errors on stderr; do not kill the session for a
			// single failed message.
			fmt.Fprintf(stderr(), "[rise-mcp-bridge] forward error: %v\n", err)
		} else {
			err = fmt.Errorf("the server sent no response")
		}
		// Every request must get an answer, or Claude waits for it until its own
		// timeout and reports only that the computer did not respond.
		for _, h := range requests {
			if p.pending[string(h.ID)] {
				p.writeError(h.ID, err)
			}
		}
	}
	return scanner.Err()
}

func (p *Pump) forward(ctx context.Context, msg []byte, isInit bool) error {
	if p.needInit && !isInit && p.initMsg != nil {
		if err := p.reinitialize(ctx); err != nil {
			return fmt.Errorf("could not open a session: %w", err)
		}
	}
	resp, tok, err := p.post(ctx, msg)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if inv, ok := p.tokenSource.(tokenInvalidator); ok {
			// The server rejected this token: drop it and send the message once
			// more with a fresh one.
			resp.Body.Close()
			inv.invalidate(tok)
			if resp, _, err = p.post(ctx, msg); err != nil {
				return err
			}
		}
	}
	// 404 on a message carrying a session id means the server has forgotten the
	// session (Metabase: "Invalid or expired session"). Open a new one and send
	// the message again.
	if resp.StatusCode == http.StatusNotFound && p.sessionID != "" && !isInit && p.initMsg != nil {
		resp.Body.Close()
		if err := p.reinitialize(ctx); err != nil {
			return fmt.Errorf("session expired and a new one could not be opened: %w", err)
		}
		if resp, _, err = p.post(ctx, msg); err != nil {
			return err
		}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusAccepted:
		// 202: notification/response accepted, no body to relay.
		return nil
	case resp.StatusCode >= 400:
		return statusError(resp)
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
		return p.relay(bytes.TrimSpace(body))
	default:
		// Some servers return empty 200s; nothing to relay.
		return nil
	}
}

// post sends one client message upstream and records the session id the server
// assigns. It returns the token it sent (nil without auth). The caller closes
// the response body.
func (p *Pump) post(ctx context.Context, msg []byte) (*http.Response, *oauth2.Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(msg))
	if err != nil {
		return nil, nil, err
	}
	var tok *oauth2.Token
	// tokenSource is nil when auth == "none".
	if p.tokenSource != nil {
		if tok, err = p.tokenSource.Token(); err != nil {
			return nil, nil, fmt.Errorf("get token: %w", err)
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
		return nil, nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		p.sessionID = sid
	}
	return resp, tok, nil
}

// reinitialize replays the client's initialize handshake to open a new session,
// discarding the server's replies: Claude already has them from the first time.
func (p *Pump) reinitialize(ctx context.Context) error {
	p.sessionID = ""
	for _, msg := range [][]byte{p.initMsg, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)} {
		resp, _, err := p.post(ctx, msg)
		if err != nil {
			p.needInit = true
			return err
		}
		if resp.StatusCode >= 400 {
			err := statusError(resp)
			resp.Body.Close()
			p.needInit = true
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	p.needInit = false
	fmt.Fprintln(stderr(), "[rise-mcp-bridge] opened a new session with the server")
	return nil
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// relay writes one server message to stdout and marks any responses in it as
// answered.
func (p *Pump) relay(msg []byte) error {
	for _, h := range parseHeads(msg) {
		if h.isResponse() {
			delete(p.pending, string(h.ID))
		}
	}
	return p.writeLine(msg)
}

// writeError answers request id with a JSON-RPC internal error carrying err.
func (p *Pump) writeError(id json.RawMessage, err error) {
	reply, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32603,
			"message": "rise-mcp-bridge: " + err.Error(),
		},
	})
	_ = p.writeLine(reply)
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
		return p.relay([]byte(payload))
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
