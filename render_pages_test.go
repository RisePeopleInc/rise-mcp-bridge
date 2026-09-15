package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderSetupPages executes both setup templates with real data, checks the
// key user-facing phrases are present, and (when RENDER_OUT is set) writes the
// HTML so the pages can be eyeballed in a browser.
func TestRenderSetupPages(t *testing.T) {
	out := os.Getenv("RENDER_OUT")
	if out == "" {
		out = t.TempDir()
	}
	var form, success strings.Builder
	if err := setupPage.Execute(&form, setupView{pageCommon: newPage(2), Host: defaultProxyHost, User: "steve_bond"}); err != nil {
		t.Fatalf("setup page: %v", err)
	}
	if err := successPage.Execute(&success, successView{pageCommon: newPage(3), Note: "note"}); err != nil {
		t.Fatalf("success page: %v", err)
	}
	for _, want := range []string{"how to set up SmartProxy", "fully quit and reopened"} {
		if !strings.Contains(form.String(), want) {
			t.Errorf("setup page missing %q", want)
		}
	}
	for _, want := range []string{"Open Metabase", "Check your browser can open Metabase", "Fully quit Claude, then reopen it", "Claude icon in the Dock", "Task Manager", "will <b>not</b> connect", "class=\"note\">note"} {
		if !strings.Contains(success.String(), want) {
			t.Errorf("success page missing %q", want)
		}
	}
	for name, html := range map[string]string{"form.html": form.String(), "success.html": success.String()} {
		if err := os.WriteFile(filepath.Join(out, name), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
