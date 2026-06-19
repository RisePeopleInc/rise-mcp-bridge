package main

import (
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// defaultProxyHost is Rise's HTTPS proxy (shared across all proxy-gated plugins).
// Pre-filled in the setup form; the user only supplies username + password.
const defaultProxyHost = "rise-proxy-ca-west-1.rise.xyz:65180"

// runSetup installs the bridge into configDir and collects the shared proxy
// credentials via a local browser form, writing <configDir>/config.json. No
// terminal, no agent file-write — this runs as a normal user process (e.g. the
// double-clicked app), which can write the user's own home dir.
func runSetup(configDir string) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	if err := selfInstall(configDir); err != nil {
		fmt.Fprintf(os.Stderr, "[rise-mcp-bridge] note: could not self-install the binary (%v); continuing with config only\n", err)
	}

	existing, _ := loadShared(configDir)
	prefHost := defaultProxyHost
	prefUser := ""
	if existing != nil {
		if strings.TrimSpace(existing.ProxyHost) != "" {
			prefHost = existing.ProxyHost
		}
		prefUser = existing.ProxyUser
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	formURL := fmt.Sprintf("http://%s/", ln.Addr().String())

	done := make(chan error, 1)
	srv := &http.Server{Handler: setupHandler(configDir, prefHost, prefUser, done)}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(os.Stderr, "\n[rise-mcp-bridge] Opening setup in your browser…\n  If it doesn't open, visit: %s\n\n", formURL)
	openBrowser(formURL)

	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-time.After(10 * time.Minute):
		return fmt.Errorf("setup timed out waiting for the form")
	}

	revealFolder(configDir)
	fmt.Fprintf(os.Stderr, "[rise-mcp-bridge] Setup complete (%s). Reload the plugin (/reload-plugins) or start a new session.\n", configDir)
	return nil
}

var setupPage = template.Must(template.New("setup").Parse(`<!doctype html><html><head><meta charset="utf-8">
<title>Rise MCP Bridge — setup</title><style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;max-width:480px;margin:48px auto;padding:0 20px;color:#1a1a1a}
h1{font-size:20px;margin-bottom:4px} .note{color:#666;font-size:13px;margin-top:0}
label{display:block;margin:18px 0 4px;font-weight:600;font-size:14px}
input{width:100%;padding:10px;font-size:15px;border:1px solid #ccc;border-radius:8px;box-sizing:border-box}
button{margin-top:24px;padding:11px 18px;font-size:15px;border:0;border-radius:8px;background:#1a73e8;color:#fff;cursor:pointer}
</style></head><body>
<h1>Connect to Rise data</h1>
<p class="note">Enter the same proxy username &amp; password your SmartProxy browser extension uses. They're stored only on this computer.</p>
<form method="POST" action="/save">
<label>Proxy server</label><input name="host" value="{{.Host}}">
<label>Username</label><input name="user" value="{{.User}}" autofocus>
<label>Password</label><input name="pass" type="password">
<button type="submit">Save &amp; connect</button>
</form></body></html>`))

func setupHandler(configDir, prefHost, prefUser string, done chan<- error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_ = setupPage.Execute(w, map[string]string{"Host": prefHost, "User": prefUser})
	})
	mux.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		host := strings.TrimSpace(r.FormValue("host"))
		user := strings.TrimSpace(r.FormValue("user"))
		pass := r.FormValue("pass")
		if host == "" || user == "" || pass == "" {
			http.Error(w, "All fields are required — go back and try again.", http.StatusBadRequest)
			return
		}
		if err := saveShared(configDir, &SharedConfig{ProxyHost: host, ProxyUser: user, ProxyPass: pass}); err != nil {
			http.Error(w, "Could not save config: "+err.Error(), http.StatusInternalServerError)
			done <- err
			return
		}
		io.WriteString(w, `<!doctype html><meta charset="utf-8"><body style="font-family:-apple-system,'Segoe UI',sans-serif;max-width:480px;margin:48px auto;padding:0 20px"><h1 style="color:#137333">&#10003; Connected</h1><p>Rise MCP Bridge is set up. Close this tab and return to Claude — reload the plugin and it'll connect (a Metabase sign-in tab opens once on first use).</p></body>`)
		done <- nil
	})
	return mux
}

// selfInstall copies the running executable into configDir as rise-mcp-bridge[.exe]
// so a plugin's .mcp.json can launch it from the stable location. No-op if already
// running from there.
func selfInstall(configDir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, e := filepath.EvalSymlinks(self); e == nil {
		self = resolved
	}
	// On macOS we're running as the .app's main executable
	// (…/RiseMCPBridge.app/Contents/MacOS/RiseMCPBridge). That binary is signed in
	// bundle context — its signature is bound to the bundle Info.plist, so a bare
	// copy is invalid standalone and AMFI kills it ("invalid Info.plist…"). The app
	// ships a second, standalone-signed copy next to it (…/MacOS/rise-mcp-bridge);
	// install that one. Other platforms (and a non-bundle run) just install self.
	if payload := filepath.Join(filepath.Dir(self), "rise-mcp-bridge"); payload != self {
		if fi, e := os.Stat(payload); e == nil && fi.Mode().IsRegular() {
			self = payload
		}
	}
	name := "rise-mcp-bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dest := filepath.Join(configDir, name)
	if d, e := filepath.EvalSymlinks(dest); e == nil && d == self {
		return nil
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	dequarantine(dest)
	return nil
}

// dequarantine strips com.apple.quarantine from a file we just installed. macOS
// propagates the quarantine flag to files written by a quarantined app (the
// downloaded installer), and a quarantined bare binary exec'd outside
// LaunchServices can be killed by Gatekeeper. Our binary is notarized, so
// de-quarantining our own installed copy is safe. Best-effort; no-op off macOS.
func dequarantine(path string) {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("xattr", "-d", "com.apple.quarantine", path).Run()
}

// revealFolder opens the install dir in the OS file browser (so the user can see
// where things landed). Best-effort.
func revealFolder(dir string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", dir).Start()
	case "windows":
		_ = exec.Command("explorer", dir).Start()
	default:
		_ = exec.Command("xdg-open", dir).Start()
	}
}
