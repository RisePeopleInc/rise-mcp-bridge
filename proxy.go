package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

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
