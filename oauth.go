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

// Fixed loopback redirect so the registered redirect_uri exactly matches the one
// sent at authorize time -- the authorization server validates this strictly.
const (
	loopbackAddr     = "127.0.0.1:47000"
	loopbackRedirect = "http://127.0.0.1:47000/callback"
)

// Fallback agent scopes if the server's discovery metadata doesn't advertise
// scopes_supported. These are the scopes a dynamically-registered MCP client is
// granted by default; without them the MCP tools/list is filtered to empty.
var defaultAgentScopes = []string{
	"agent:search", "agent:query", "agent:sql:*", "agent:notebook:*",
	"agent:viz:*", "agent:dashboard:*", "agent:document:*", "agent:alert:*",
	"agent:resource:*", "agent:todo:*", "agent:metadata:*", "agent:question:*",
	"agent:transforms:*", "agent:snippets:*",
}

type tokenStore struct {
	dir string
	key string // per-endpoint suffix so multiple plugins/tools cache independently
}

func (t tokenStore) path() string         { return filepath.Join(t.dir, "token-"+t.key+".json") }
func (t tokenStore) clientIDPath() string { return filepath.Join(t.dir, "client_id-"+t.key) }

// endpointKey derives a short, filesystem-safe key from the MCP endpoint URL, so
// each target endpoint gets its own cached token + dynamic client registration.
func endpointKey(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

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
	return os.WriteFile(t.path(), raw, 0o600)
}

type authServerMeta struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
}

func discover(ctx context.Context, client *http.Client, mcpEndpoint string) (*authServerMeta, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, mcpEndpoint, nil)
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	} else {
		return nil, fmt.Errorf("probe mcp endpoint via proxy: %w", err)
	}

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

func Authenticate(ctx context.Context, client *http.Client, mcpEndpoint string, store tokenStore) (oauth2.TokenSource, error) {
	meta, err := discover(ctx, client, mcpEndpoint)
	if err != nil {
		return nil, err
	}

	clientID, err := ensureClientID(ctx, client, meta, store, loopbackRedirect)
	if err != nil {
		return nil, err
	}

	// Request the agent scopes, or the MCP tools/list comes back filtered to empty.
	scopes := meta.ScopesSupported
	if len(scopes) == 0 {
		scopes = defaultAgentScopes
	}

	conf := &oauth2.Config{
		ClientID:    clientID,
		RedirectURL: loopbackRedirect,
		Scopes:      scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  meta.AuthorizationEndpoint,
			TokenURL: meta.TokenEndpoint,
		},
	}

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

func ensureClientID(ctx context.Context, client *http.Client, meta *authServerMeta, store tokenStore, redirectURI string) (string, error) {
	idPath := store.clientIDPath()
	if b, err := os.ReadFile(idPath); err == nil && len(b) > 0 {
		return string(b), nil
	}
	if meta.RegistrationEndpoint == "" {
		return "", fmt.Errorf("no registration_endpoint advertised and no cached client_id")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Rise MCP Bridge",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
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

func loopbackPKCE(ctx context.Context, client *http.Client, conf *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", loopbackAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s (another sign-in in progress?): %w", loopbackAddr, err)
	}
	defer ln.Close()

	verifier := oauth2.GenerateVerifier()
	state := randString(24)
	authURL := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))

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
		fmt.Fprintln(w, "Signed in. You can close this tab and return to Claude.")
		codeCh <- r.URL.Query().Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(os.Stderr, "\n[rise-mcp-bridge] Opening your browser to sign in...\n  If it doesn't open, visit:\n  %s\n\n", authURL)
	openBrowser(authURL)

	select {
	case code := <-codeCh:
		return conf.Exchange(ctxWithClient(ctx, client), code, oauth2.VerifierOption(verifier))
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for sign-in authorization")
	}
}

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

func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
