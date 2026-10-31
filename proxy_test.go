package main

import (
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// tunnelProxy is an HTTPS CONNECT proxy like the Rise proxy. Once blackhole is
// set, it stops passing bytes but keeps both sockets open, the way a
// connection looks after the computer slept or the network changed.
type tunnelProxy struct{ blackhole atomic.Bool }

func (p *tunnelProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	up, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	down, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer down.Close()
	done := make(chan struct{}, 2)
	go func() { p.pipe(up, down); done <- struct{}{} }()
	go func() { p.pipe(down, up); done <- struct{}{} }()
	<-done
}

func (p *tunnelProxy) pipe(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if p.blackhole.Load() {
			continue
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

// proxiedSetup starts an HTTP/2 upstream running handler behind a tunnelProxy
// and returns a client from newProxiedClient that reaches it through the proxy.
func proxiedSetup(t *testing.T, handler http.HandlerFunc) (*http.Client, string, *tunnelProxy) {
	t.Helper()
	old := healthCheckAfter
	healthCheckAfter = 200 * time.Millisecond
	t.Cleanup(func() { healthCheckAfter = old })

	up := httptest.NewUnstartedServer(handler)
	up.EnableHTTP2 = true
	up.StartTLS()
	t.Cleanup(up.Close)

	tp := &tunnelProxy{}
	px := httptest.NewTLSServer(tp)
	t.Cleanup(px.Close)

	// Both test servers present httptest's one built-in certificate.
	ca := filepath.Join(t.TempDir(), "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw})
	if err := os.WriteFile(ca, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := newProxiedClient(px.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	return client, up.URL, tp
}

// A query whose response headers arrive only when it finishes, as with
// Metabase's execute_sql, must not be cut off however long it runs (0.2.13 cut
// it off at 30 s). Here it runs ten health-check periods.
func TestSlowResponseOnLiveConnectionSucceeds(t *testing.T) {
	client, url, _ := proxiedSetup(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		io.WriteString(w, "done")
	})
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("slow response failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("got %s, want HTTP/2 (the health check is an HTTP/2 ping)", resp.Proto)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "done" {
		t.Fatalf("body = %q", b)
	}
}

// A connection that goes silent while a request waits must fail within a
// couple of health-check periods, so the request gets an error reply instead
// of hanging past Claude's own wait.
func TestDeadConnectionFailsFast(t *testing.T) {
	release := make(chan struct{})
	var deadNow func()
	client, url, tp := proxiedSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			return
		}
		deadNow()
		<-release
	})
	deadNow = func() { tp.blackhole.Store(true) }
	// Registered after proxiedSetup, so it runs first and the server can close.
	t.Cleanup(func() { close(release) })
	// Open the connection first, so the black hole hits a request in flight.
	resp, err := client.Get(url + "/warm")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	start := time.Now()
	errc := make(chan error, 1)
	go func() {
		resp, err := client.Get(url + "/slow")
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("request on a dead connection succeeded")
		}
		t.Logf("failed after %v: %v", time.Since(start).Round(time.Millisecond), err)
	case <-time.After(5 * time.Second):
		t.Fatal("request on a dead connection still hanging after 5 s")
	}
}
