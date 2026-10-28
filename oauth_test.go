package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// rotatingAuthServer issues a new refresh token on every refresh and refuses a
// spent one, like Metabase's authorization server.
type rotatingAuthServer struct {
	mu        sync.Mutex
	live      string
	refreshes int
}

func (a *rotatingAuthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.FormValue("refresh_token") != a.live {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_request"}`)
		return
	}
	a.refreshes++
	a.live = fmt.Sprintf("refresh-%d", a.refreshes)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": fmt.Sprintf("access-%d", a.refreshes), "token_type": "Bearer",
		"refresh_token": a.live, "expires_in": 3600,
	})
}

func newTestSource(t *testing.T, srv *httptest.Server, store tokenStore, tok *oauth2.Token) *sharedTokenSource {
	conf := &oauth2.Config{ClientID: "c", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
	return newSharedTokenSource(t.Context(), conf, store, tok, "https://mcp.example/mcp", "")
}

func expired(refresh string) *oauth2.Token {
	return &oauth2.Token{AccessToken: "stale", RefreshToken: refresh, Expiry: time.Now().Add(-time.Hour)}
}

func noSignIn(t *testing.T) {
	t.Helper()
	orig := startSignIn
	startSignIn = func(tokenStore, string, string) error { t.Error("unexpected browser sign-in"); return nil }
	t.Cleanup(func() { startSignIn = orig })
}

// The Cowork case: a long-lived bridge still holds a refresh token another
// bridge has since spent. It must use the other bridge's token from the file.
func TestSharedSourceUsesTokenAnotherBridgeRefreshed(t *testing.T) {
	noSignIn(t)
	auth := &rotatingAuthServer{live: "refresh-0"}
	srv := httptest.NewServer(auth)
	defer srv.Close()
	store := tokenStore{dir: t.TempDir(), key: "k"}

	longLived := newTestSource(t, srv, store, expired("refresh-0"))
	other := newTestSource(t, srv, store, expired("refresh-0"))
	if _, err := other.Token(); err != nil {
		t.Fatal(err)
	}
	tok, err := longLived.Token()
	if err != nil {
		t.Fatalf("long-lived bridge failed: %v", err)
	}
	if tok.AccessToken != "access-1" || auth.refreshes != 1 {
		t.Fatalf("want the other bridge's token and one refresh, got %q after %d", tok.AccessToken, auth.refreshes)
	}
}

func TestConcurrentBridgesRefreshOnce(t *testing.T) {
	noSignIn(t)
	auth := &rotatingAuthServer{live: "refresh-0"}
	srv := httptest.NewServer(auth)
	defer srv.Close()
	store := tokenStore{dir: t.TempDir(), key: "k"}
	if err := store.save(expired("refresh-0")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = newTestSource(t, srv, store, expired("refresh-0")).Token()
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if auth.refreshes != 1 {
		t.Fatalf("want exactly one refresh across bridges, got %d", auth.refreshes)
	}
	if left, _ := filepath.Glob(filepath.Join(store.dir, "*.part")); len(left) > 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
	if _, err := os.Stat(store.path() + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock file left behind: %v", err)
	}
}

func TestRejectedTokenIsNotReused(t *testing.T) {
	noSignIn(t)
	auth := &rotatingAuthServer{live: "refresh-0"}
	srv := httptest.NewServer(auth)
	defer srv.Close()
	store := tokenStore{dir: t.TempDir(), key: "k"}
	valid := &oauth2.Token{AccessToken: "revoked", RefreshToken: "refresh-0", Expiry: time.Now().Add(time.Hour)}
	if err := store.save(valid); err != nil {
		t.Fatal(err)
	}
	src := newTestSource(t, srv, store, valid)
	src.invalidate(valid)
	tok, err := src.Token()
	if err != nil || tok.AccessToken != "access-1" {
		t.Fatalf("want a refreshed token, got %v, %v", tok, err)
	}
}

func TestRefusedRefreshStartsSignIn(t *testing.T) {
	auth := &rotatingAuthServer{live: "someone-elses"}
	srv := httptest.NewServer(auth)
	defer srv.Close()
	started := 0
	orig := startSignIn
	startSignIn = func(tokenStore, string, string) error { started++; return nil }
	defer func() { startSignIn = orig }()
	if !loopbackFree() {
		t.Skip("sign-in port in use on this machine")
	}

	src := newTestSource(t, srv, tokenStore{dir: t.TempDir(), key: "k"}, expired("spent"))
	for range 2 {
		if _, err := src.Token(); err == nil || !strings.Contains(err.Error(), "sign in") {
			t.Fatalf("want a sign-in-again error, got %v", err)
		}
	}
	if started != 1 {
		t.Fatalf("want one browser sign-in for repeated failures, got %d", started)
	}
}

func TestServerErrorDoesNotStartSignIn(t *testing.T) {
	noSignIn(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	src := newTestSource(t, srv, tokenStore{dir: t.TempDir(), key: "k"}, expired("refresh-0"))
	if _, err := src.Token(); err == nil {
		t.Fatal("want an error")
	}
}
