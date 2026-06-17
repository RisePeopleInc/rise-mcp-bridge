package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/oauth2"
)

// ----------------------------------------------------------------------------
// OAuth 2.0 against Metabase's embedded OAuth server.
//
// Metabase 60's MCP server authenticates MCP clients with OAuth 2.0 (PKCE,
// dynamic client registration, loopback redirect). All of these HTTP calls go
// through the SAME proxied client (newProxiedClient), so token acquisition also
// egresses from the allowlisted proxy IP.
//
// Flow (first connection):
//   1. Discover the protected-resource / authorization-server metadata.
//      Discovery starts from the WWW-Authenticate header Metabase returns on an
//      unauthenticated request to the MCP endpoint (resource_metadata URL is
//      built from Metabase's Site URL — hence config.MetabaseURL must match it).
//   2. Dynamically register this client (RFC 7591) if no client_id is cached.
//   3. Open the user's browser to the authorization endpoint (PKCE challenge),
//      with a loopback redirect_uri on 127.0.0.1:<random port>.
//   4. Capture the code on the loopback listener, exchange for tokens.
//   5. Persist tokens to <configDir>/token.json and refresh as needed.
//
// >>> VALIDATION SPIKE (Rise eng): the discovery + dynamic-registration request
// >>> shapes below must be confirmed against the live Metabase instance. The
// >>> endpoints and field names follow the MCP authorization spec and Metabase
// >>> docs, but the exact metadata URLs are instance-specific. See
// >>> bridge/README.md. Everything routes through `client` (proxied).
// ----------------------------------------------------------------------------

type tokenStore struct {
	dir string
}

func (t tokenStore) path() string { return filepath.Join(t.dir, "token.json") }

func (t tokenStore) load() (*oauth2.Token, bool) {
	raw, err := os.ReadFile(t.path())
	if err != nil {
		return nil, false
	}
	var tok oauth2.Token
	if json.Unmarshal(raw, &tok) != nil {
		return nil, false
	}
	return &tok, true
}

func (t tokenStore) save(tok *oauth2.Token) error {
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	// 0600: tokens are per-user secrets.
	return os.WriteFile(t.path(), raw, 0o600)
}

// authServerMeta is the subset of RFC 8414 authorization-server metadata we use.
type authServerMeta struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// discover fetches the authorization-server metadata via the proxied client.
//
// VALIDATE: confirm the metadata path Metabase advertises. The MCP auth spec
// uses the WWW-Authenticate `resource_metadata` link on a 401 from the MCP
// endpoint, which in turn points at the auth-server metadata document.
func discover(ctx context.Context, client *http.Client, mcpEndpoint string) (*authServerMeta, error) {
	// Probe the MCP endpoint unauthenticated to read WWW-Authenticate.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, mcpEndpoint, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe mcp endpoint via proxy: %w", err)
	}
	defer resp.Body.Close()

	// TODO(validate): parse resp.Header["Www-Authenticate"] -> resource_metadata
	// URL -> fetch -> authorization_servers[0] -> fetch RFC 8414 doc. Fall back
	// to the conventional /.well-known/oauth-authorization-server on the
	// Metabase origin if the header path is absent.
	metaURL := originOf(mcpEndpoint) + "/.well-known/oauth-authorization-server"
	mreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	mresp, err := client.Do(mreq)
	if err != nil {
		return nil, fmt.Errorf("fetch auth-server metadata via proxy: %w", err)
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth-server metadata: unexpected status %d from %s", mresp.StatusCode, metaURL)
	}
	var m authServerMeta
	if err := json.NewDecoder(mresp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode auth-server metadata: %w", err)
	}
	return &m, nil
}

// Authenticate returns a token source backed by the proxied client, performing
// the interactive loopback PKCE flow on first run and reusing/refreshing the
// cached token thereafter.
func Authenticate(ctx context.Context, client *http.Client, cfg *Config, store tokenStore) (oauth2.TokenSource, error) {
	meta, err := discover(ctx, client, cfg.MCPEndpoint)
	if err != nil {
		return nil, err
	}

	clientID, err := ensureClientID(ctx, client, meta, store)
	if err != nil {
		return nil, err
	}

	conf := &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{
			AuthURL:  meta.AuthorizationEndpoint,
			TokenURL: meta.TokenEndpoint,
		},
		// Scopes: per Metabase MCP — the granted token is scoped to the user's
		// Metabase permissions regardless; leave empty unless the instance
		// advertises required scopes in metadata.
	}

	// Reuse cached token if present.
	if tok, ok := store.load(); ok {
		return persistingSource{store: store, src: conf.TokenSource(ctxWithClient(ctx, client), tok)}, nil
	}

	tok, err := loopbackPKCE(ctx, client, conf)
	if err != nil {
		return nil, err
	}
	if err := store.save(tok); err != nil {
		return nil, err
	}
	return persistingSource{store: store, src: conf.TokenSource(ctxWithClient(ctx, client), tok)}, nil
}

// ensureClientID returns a cached dynamic-registration client_id or registers a
// new one (RFC 7591) via the proxied client.
func ensureClientID(ctx context.Context, client *http.Client, meta *authServerMeta, store tokenStore) (string, error) {
	idPath := filepath.Join(store.dir, "client_id")
	if b, err := os.ReadFile(idPath); err == nil && len(b) > 0 {
		return string(b), nil
	}
	if meta.RegistrationEndpoint == "" {
		return "", fmt.Errorf("no registration_endpoint advertised and no cached client_id; see bridge/README.md")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Rise MCP Bridge",
		"redirect_uris":              []string{"http://127.0.0.1/callback"}, // host fixed up at auth time
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none", // public client + PKCE
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, meta.RegistrationEndpoint, bytesReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("dynamic client registration via proxy: %w", err)
	}
	defer resp.Body.Close()
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil || reg.ClientID == "" {
		return "", fmt.Errorf("dynamic client registration: bad response (status %d)", resp.StatusCode)
	}
	_ = os.WriteFile(idPath, []byte(reg.ClientID), 0o600)
	return reg.ClientID, nil
}

// loopbackPKCE runs the interactive authorization-code-with-PKCE flow using a
// transient 127.0.0.1 listener as the redirect target, opening the user's
// browser to the Metabase consent page.
func loopbackPKCE(ctx context.Context, client *http.Client, conf *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	conf.RedirectURL = fmt.Sprintf("http://%s/callback", ln.Addr().String())

	verifier := oauth2.GenerateVerifier()
	state := randString(24)
	authURL := conf.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.AccessTypeOffline,
	)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization error: %s", e)
			return
		}
		fmt.Fprintln(w, "Rise Metabase connected. You can close this tab and return to Claude.")
		codeCh <- r.URL.Query().Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// Prompt is written to stderr so it never corrupts the stdio MCP stream.
	fmt.Fprintf(os.Stderr, "\n[rise-metabase] Opening your browser to sign in to Metabase…\n  If it doesn't open, visit:\n  %s\n\n", authURL)
	openBrowser(authURL)

	select {
	case code := <-codeCh:
		return conf.Exchange(ctxWithClient(ctx, client), code, oauth2.VerifierOption(verifier))
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for Metabase authorization")
	}
}

// persistingSource re-saves the token whenever it is refreshed.
type persistingSource struct {
	store tokenStore
	src   oauth2.TokenSource
}

func (p persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err == nil && tok != nil {
		_ = p.store.save(tok)
	}
	return tok, err
}

// ctxWithClient injects the proxied client so oauth2 token/exchange calls also
// transit the Rise proxy.
func ctxWithClient(ctx context.Context, client *http.Client) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, client)
}

func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, append(args, url)...).Start()
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkceS256 is provided for reference; oauth2.S256ChallengeOption handles this.
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
