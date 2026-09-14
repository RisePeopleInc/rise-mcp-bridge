package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRenderSetupPages executes both setup templates with realistic data and
// writes the HTML to RENDER_OUT (when set) so they can be eyeballed in a browser.
func TestRenderSetupPages(t *testing.T) {
	out := os.Getenv("RENDER_OUT")
	form := filepath.Join(t.TempDir(), "form.html")
	success := filepath.Join(t.TempDir(), "success.html")
	if out != "" {
		form, success = filepath.Join(out, "form.html"), filepath.Join(out, "success.html")
	}
	f, err := os.Create(form)
	if err != nil {
		t.Fatal(err)
	}
	if err := setupPage.Execute(f, setupView{Logo: riseLogo, Host: defaultProxyHost, User: "steve_bond", Steps: steps(2), Tools: proxyGatedTools, ProxyHowTo: proxyHowToURL}); err != nil {
		t.Fatalf("setup page: %v", err)
	}
	f.Close()
	g, err := os.Create(success)
	if err != nil {
		t.Fatal(err)
	}
	if err := successPage.Execute(g, successView{Logo: riseLogo, Steps: steps(3), Tools: proxyGatedTools, ProxyHowTo: proxyHowToURL}); err != nil {
		t.Fatalf("success page: %v", err)
	}
	g.Close()
}
