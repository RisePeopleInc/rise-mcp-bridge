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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"golang.org/x/oauth2"
)

// Fixed loopback redirect so the registered redirect_uri exactly matches the one
// sent at authorize time -- the authorization server validates this strictly.
// The listener doubles as a lock: while a sign-in is in progress the port is held,
// so a second bridge instance knows not to open another browser tab.
const (
	loopbackAddr     = "127.0.0.1:47000"
	loopbackRedirect = "http://127.0.0.1:47000/callback"

	// How long a browser sign-in may take. Corporate proxies in the browser can add
	// minutes before the login page even renders, so be generous.
	signInTimeout = 15 * time.Minute
)

// Fallback agent scopes if neither the MCP resource nor the auth server advertises
// scopes. Without agent scopes the MCP tools/list is filtered to empty.
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
func (t tokenStore) loginLogPath() string { return filepath.Join(t.dir, "login-"+t.key+".log") }

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
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		return nil, false
	}
	return &tok, true
}

// save writes the token atomically (temp file + rename) so a concurrent reader --
// the bridge instance waiting on the detached sign-in helper -- never sees a
// half-written file.
func (t tokenStore) save(tok *oauth2.Token) error {
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path() + ".part"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.path())
}

// authServerMeta is what we need from the authorization server's RFC 8414
// metadata, plus what the MCP endpoint says about itself (RFC 9728 protected
// resource metadata), when it publishes any.
type authServerMeta struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`

	// From the protected-resource metadata (not part of the auth-server document).
	Resource       string
	ResourceScopes []string
}

type resourceMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

var resourceMetadataRe = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// discover probes the MCP endpoint (through the proxy), reads the RFC 9728
// resource metadata it points at, and then the authorization server's metadata.
//
// The resource metadata matters: it lists the scopes the MCP surface actually
// accepts. The auth server's own scopes_supported can include scopes for other
// surfaces (Metabase advertises `mb:full`, its REST-API scope, there). A
// dynamically-registered client asking for a scope outside what it may request
// is rejected by the server with a bare "invalid_request", so we ask only for
// what the resource we are about to talk to advertises.
func discover(ctx context.Context, client *http.Client, mcpEndpoint string) (*authServerMeta, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, mcpEndpoint, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe mcp endpoint via proxy: %w", err)
	}
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()

	rm := fetchResourceMeta(ctx, client, mcpEndpoint, wwwAuth)

	authOrigin := originOf(mcpEndpoint)
	if rm != nil && len(rm.AuthorizationServers) > 0 && rm.AuthorizationServers[0] != "" {
		authOrigin = trimSlash(rm.AuthorizationServers[0])
	}
	metaURL := authOrigin + "/.well-known/oauth-authorization-server"
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
	if rm != nil {
		m.Resource = rm.Resource
		m.ResourceScopes = rm.ScopesSupported
	}
	return &m, nil
}

// fetchResourceMeta returns the MCP endpoint's protected-resource metadata, from
// the URL named in its WWW-Authenticate challenge or the RFC 9728 well-known
// location. Best-effort: nil when the server publishes none.
func fetchResourceMeta(ctx context.Context, client *http.Client, mcpEndpoint, wwwAuth string) *resourceMeta {
	candidates := []string{}
	if m := resourceMetadataRe.FindStringSubmatch(wwwAuth); len(m) == 2 {
		candidates = append(candidates, m[1])
	}
	if u, err := url.Parse(mcpEndpoint); err == nil {
		candidates = append(candidates, originOf(mcpEndpoint)+"/.well-known/oauth-protected-resource"+u.Path)
	}
	for _, c := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var rm resourceMeta
		decErr := json.NewDecoder(resp.Body).Decode(&rm)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && decErr == nil && rm.Resource != "" {
			return &rm
		}
	}
	return nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// requestScopes picks what to ask for: the resource's own list first, then the
// auth server's, then the built-in fallback.
func requestScopes(meta *authServerMeta) []string {
	if len(meta.ResourceScopes) > 0 {
		return meta.ResourceScopes
	}
	if len(meta.ScopesSupported) > 0 {
		return meta.ScopesSupported
	}
	return defaultAgentScopes
}

func oauthConfig(meta *authServerMeta, clientID string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    clientID,
		RedirectURL: loopbackRedirect,
		Scopes:      requestScopes(meta),
		Endpoint: oauth2.Endpoint{
			AuthURL:  meta.AuthorizationEndpoint,
			TokenURL: meta.TokenEndpoint,
		},
	}
}

// resourceOpt is the RFC 8707 resource indicator, sent on authorize and token
// requests when the resource published metadata (servers that support indicators
// narrow the consent + token to that surface).
func resourceOpt(meta *authServerMeta) []oauth2.AuthCodeOption {
	if meta.Resource == "" {
		return nil
	}
	return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("resource", meta.Resource)}
}

// Authenticate returns a token source for mcpEndpoint, signing the user in through
// the browser on first use.
//
// How the sign-in runs depends on who launched us. Run by hand in a terminal
// (inline), it happens in this process. Launched by an MCP host (the normal case),
// it runs in a *detached helper process*: the host gives a server 60-120 s to
// answer its first request and then kills it, which is far less than a browser
// sign-in through a corporate proxy can take. Killing this process must not kill
// the sign-in, so the helper owns the browser flow and writes the token file; we
// wait for that file, and if the host kills us first, the next launch finds the
// token and connects immediately. The loopback port acts as the lock: if a helper
// already holds it, we just wait rather than opening a second browser tab.
func Authenticate(ctx context.Context, client *http.Client, mcpEndpoint string, store tokenStore, inline bool, caFile string) (oauth2.TokenSource, error) {
	meta, err := discover(ctx, client, mcpEndpoint)
	if err != nil {
		return nil, err
	}
	clientID, err := ensureClientID(ctx, client, meta, store, loopbackRedirect)
	if err != nil {
		return nil, err
	}
	conf := oauthConfig(meta, clientID)

	if tok, ok := store.load(); ok {
		return persistingSource{store: store, src: conf.TokenSource(ctxWithClient(ctx, client), tok)}, nil
	}

	var tok *oauth2.Token
	if inline {
		tok, err = loopbackPKCE(ctx, client, conf, meta)
		if err != nil {
			return nil, err
		}
		if err := store.save(tok); err != nil {
			return nil, err
		}
	} else {
		if loopbackFree() {
			if err := spawnLoginHelper(store, mcpEndpoint, caFile); err != nil {
				return nil, fmt.Errorf("start sign-in helper: %w", err)
			}
			fmt.Fprintf(os.Stderr, "[rise-mcp-bridge] Sign-in required: opening your browser (log: %s).\n", store.loginLogPath())
		} else {
			fmt.Fprintln(os.Stderr, "[rise-mcp-bridge] A sign-in is already in progress in your browser; waiting for it.")
		}
		fmt.Fprintln(os.Stderr, "[rise-mcp-bridge] If Claude gives up waiting, finish signing in anyway, then start a new chat — the sign-in keeps running.")
		tok, err = waitForToken(ctx, store)
		if err != nil {
			return nil, err
		}
	}
	return persistingSource{store: store, src: conf.TokenSource(ctxWithClient(ctx, client), tok)}, nil
}

// runLogin is the detached helper (`--login --mcp-endpoint URL`): sign in via the
// browser and write the token file, then exit. Also usable by hand to pre-authorize.
func runLogin(configDir, mcpEndpoint, caFile string) error {
	shared, err := loadShared(configDir)
	if err != nil || !shared.hasProxyCreds() {
		return fmt.Errorf("proxy credentials not set in %s — run the Rise MCP Bridge setup first", configDir)
	}
	if caFile == "" {
		caFile = shared.CAFile
	}
	pURL, err := shared.proxyURL()
	if err != nil {
		return err
	}
	client, err := newProxiedClient(pURL, caFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), signInTimeout+time.Minute)
	defer cancel()
	store := tokenStore{dir: configDir, key: endpointKey(mcpEndpoint)}
	fmt.Fprintf(os.Stderr, "%s [rise-mcp-bridge %s] sign-in helper for %s\n", time.Now().Format(time.RFC3339), version, mcpEndpoint)
	meta, err := discover(ctx, client, mcpEndpoint)
	if err != nil {
		return err
	}
	clientID, err := ensureClientID(ctx, client, meta, store, loopbackRedirect)
	if err != nil {
		return err
	}
	tok, err := loopbackPKCE(ctx, client, oauthConfig(meta, clientID), meta)
	if err != nil {
		return err
	}
	if err := store.save(tok); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "[rise-mcp-bridge] Signed in; token saved.")
	return nil
}

// spawnLoginHelper re-executes this binary detached (own session / process group,
// no inherited stdio) so it outlives us if the MCP host kills us.
func spawnLoginHelper(store tokenStore, mcpEndpoint, caFile string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if r, e := filepath.EvalSymlinks(self); e == nil {
		self = r
	}
	args := []string{"--login", "--mcp-endpoint", mcpEndpoint, "--config-dir", store.dir}
	if caFile != "" {
		args = append(args, "--ca-file", caFile)
	}
	logf, err := os.OpenFile(store.loginLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// loopbackFree reports whether nobody currently holds the sign-in callback port.
func loopbackFree() bool {
	ln, err := net.Listen("tcp", loopbackAddr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// waitForToken polls for the token file the helper writes. It gives up early if
// the helper has exited (port released) without producing a token.
func waitForToken(ctx context.Context, store tokenStore) (*oauth2.Token, error) {
	deadline := time.Now().Add(signInTimeout)
	started := time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if tok, ok := store.load(); ok {
			return tok, nil
		}
		// Give the helper a few seconds to bind the port before treating a free
		// port as "helper gone".
		if time.Since(started) > 10*time.Second && loopbackFree() {
			if tok, ok := store.load(); ok {
				return tok, nil
			}
			return nil, fmt.Errorf("sign-in did not complete (see %s); start a new chat to try again", store.loginLogPath())
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for sign-in")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
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

func loopbackPKCE(ctx context.Context, client *http.Client, conf *oauth2.Config, meta *authServerMeta) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", loopbackAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s (another sign-in in progress?): %w", loopbackAddr, err)
	}
	defer ln.Close()

	verifier := oauth2.GenerateVerifier()
	state := randString(24)
	authURL := conf.AuthCodeURL(state, append([]oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}, resourceOpt(meta)...)...)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
			errCh <- fmt.Errorf("authorization error: %s (%s)", e, r.URL.Query().Get("error_description"))
			return
		}
		fmt.Fprintln(w, "Signed in. You can close this tab and return to Claude — if Claude had given up waiting, start a new chat.")
		codeCh <- r.URL.Query().Get("code")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(os.Stderr, "\n[rise-mcp-bridge] Opening your browser to sign in...\n  If it doesn't open, visit:\n  %s\n\n", authURL)
	openBrowser(authURL)

	select {
	case code := <-codeCh:
		return conf.Exchange(ctxWithClient(ctx, client), code, append([]oauth2.AuthCodeOption{oauth2.VerifierOption(verifier)}, resourceOpt(meta)...)...)
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(signInTimeout):
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
