package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// proxyProbeURL is a neutral, IANA-reserved public HTTPS endpoint used only to
// test that the proxy accepts the user's Basic auth. A forward proxy authenticates
// the request before attempting egress, so wrong credentials return 407 regardless
// of whether the probe host itself is reachable. The response body is irrelevant.
const proxyProbeURL = "https://example.com/"

// verifyProxyCreds round-trips a single request through the proxy with the given
// credentials to confirm they're accepted. It returns ok=false ONLY on a
// definitive proxy-auth rejection (HTTP 407); any other outcome — including an
// unreachable probe host or a flaky network — is treated as "not rejected", so a
// correct credential is never blocked by something unrelated. note is a
// human-readable status for the setup form (empty when nothing needs saying).
func verifyProxyCreds(cfg *SharedConfig) (ok bool, note string) {
	pURL, err := cfg.proxyURL()
	if err != nil {
		return false, "Proxy server looks wrong: " + err.Error()
	}
	client, err := newProxiedClient(pURL, cfg.CAFile)
	if err != nil {
		return false, err.Error()
	}
	client.Timeout = 12 * time.Second

	req, err := http.NewRequest(http.MethodGet, proxyProbeURL, nil)
	if err != nil {
		return true, "" // construction shouldn't fail; don't block the user over it
	}
	resp, err := client.Do(req)
	if err != nil {
		if isProxyAuthError(err) {
			return false, ""
		}
		return true, "Saved. Note: couldn't fully reach the test site through the proxy, but your username and password were not rejected — if the connector doesn't come online, re-open this app and double-check them."
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return false, ""
	}
	return true, ""
}

// isProxyAuthError reports whether a transport error is a proxy 407 (returned when
// the proxy rejects the CONNECT for bad/missing Basic auth). The standard library
// surfaces this as the proxy's status line in the error text.
func isProxyAuthError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "proxy authentication required") || strings.Contains(s, "407")
}

// newProxiedClient builds an *http.Client whose every request is tunnelled
// through the Rise HTTPS forward proxy.
//
// This is the part of the bridge that satisfies the core security requirement:
// because the request egresses from the proxy, it arrives at the upstream MCP
// server from the proxy's allowlisted egress IP. Nothing reaches the server
// directly.
//
// Go's http.Transport natively supports an https:// proxy (TLS-to-proxy, i.e.
// TLS-in-TLS): when Transport.Proxy returns an https URL, the standard library
// dials the proxy over TLS and then issues CONNECT for the upstream host. This
// is exactly the "secure web proxy" shape the Rise proxy requires, and it is why
// a Go bridge avoids the TLS-in-TLS limitations of many Node HTTP clients.
//
// Basic auth: when the proxy URL embeds userinfo (https://USER:PASS@host:port),
// the standard library automatically sends the Proxy-Authorization: Basic header
// on the CONNECT request. Credentials must be URL-encoded in the config (the
// setup skill does this).
//
// caFile (optional) adds a PEM bundle to the trusted roots for the UPSTREAM TLS
// connection — e.g. when the MCP server sits behind an internal CA.
func newProxiedClient(proxyRawURL, caFile string) (*http.Client, error) {
	proxyURL, err := url.Parse(proxyRawURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file %q: %w", caFile, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %q contained no usable certificates", caFile)
		}
		tlsConf.RootCAs = pool
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),

		// TLSClientConfig applies to the TLS connection to the *upstream* server.
		TLSClientConfig: tlsConf,

		// The Rise proxy must allow CONNECT tunnelling and not buffer/kill idle
		// connections (per the reference notes). Keep idle conns modest and let
		// long-lived SSE streams hold their own connection.
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	// No global client Timeout: MCP Streamable HTTP uses long-lived SSE GET
	// streams for server->client messages. Per-request deadlines are applied via
	// context where appropriate in mcphttp.go.
	return &http.Client{Transport: transport}, nil
}
