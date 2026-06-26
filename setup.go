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

// riseLogo is the official Rise wordmark (trademark Rise People Inc.), inlined so
// the setup page is fully self-contained. Injected via {{.Logo}} as template.HTML
// so html/template doesn't try to interpret the SVG's tags as page structure.
var riseLogo = template.HTML(`<svg width="124" height="32" viewBox="0 0 124 32" fill="none" xmlns="http://www.w3.org/2000/svg" role="img" aria-label="Rise People">
<path d="M69.3335 27.5554V4.44434H74.6668V27.5554H69.3335Z" fill="#505050"/>
<path d="M82.3022 24.3022L84.9689 20.5511C85.9234 21.5447 87.0691 22.3349 88.337 22.8742C89.605 23.4134 90.9689 23.6905 92.3467 23.6889C95.0667 23.6889 96.3911 22.4266 96.3911 21.1022C96.3911 16.9955 83.0578 19.8133 83.0578 11.0666C83.0578 7.19997 86.4 3.99997 91.8756 3.99997C95.1961 3.87257 98.4376 5.03162 100.924 7.23553L98.1778 10.8622C96.3223 9.20105 93.9125 8.2942 91.4222 8.31997C89.2889 8.31997 88.1067 9.26219 88.1067 10.6489C88.1067 14.3466 101.44 11.8755 101.44 20.5511C101.44 24.8 98.4 28 92.1334 28C87.6622 28 84.4445 26.4977 82.3022 24.3022Z" fill="#505050"/>
<path d="M107.556 27.5554V4.44434H123.556V9.11989H112.365V13.6443H123.325V17.9554H112.365V22.6666H123.556V27.5554H107.556Z" fill="#505050"/>
<path d="M62.2221 12.7732C62.2221 10.5704 61.3494 8.4573 59.7952 6.89635C58.2409 5.33539 56.1315 4.45374 53.9287 4.44434H42.6665V27.5554H47.5909V21.1021H52.4976L56.2576 27.5554H61.9732L57.6798 20.1954C59.0463 19.4957 60.1933 18.4326 60.9947 17.1231C61.7961 15.8136 62.2208 14.3085 62.2221 12.7732ZM47.5909 9.31544H53.9287C54.3827 9.31427 54.8325 9.40296 55.252 9.57643C55.6716 9.74989 56.0526 10.0047 56.3732 10.3261C56.6938 10.6476 56.9477 11.0293 57.12 11.4493C57.2924 11.8693 57.38 12.3192 57.3776 12.7732C57.3683 13.692 56.9973 14.5701 56.3451 15.2173C55.6929 15.8645 54.812 16.2287 53.8932 16.231H47.5909V9.31544Z" fill="#505050"/>
<path opacity="0.5" d="M9.50201 16C9.50201 14.2767 10.1866 12.624 11.4052 11.4054C12.6237 10.1868 14.2765 9.50225 15.9998 9.50225C17.7231 9.50225 19.3758 10.1868 20.5944 11.4054C21.813 12.624 22.4976 14.2767 22.4976 16C22.4976 17.7233 21.813 19.3761 20.5944 20.5946C19.3758 21.8132 17.7231 22.4978 15.9998 22.4978C14.2765 22.4978 12.6237 21.8132 11.4052 20.5946C10.1866 19.3761 9.50201 17.7233 9.50201 16ZM15.9998 4.4978C13.7249 4.4978 11.501 5.1724 9.60949 6.43628C7.71796 7.70016 6.24369 9.49656 5.37312 11.5983C4.50254 13.7001 4.27476 16.0128 4.71858 18.244C5.16239 20.4752 6.25787 22.5247 7.86649 24.1333C9.4751 25.7419 11.5246 26.8374 13.7558 27.2812C15.987 27.725 18.2997 27.4973 20.4015 26.6267C22.5032 25.7561 24.2996 24.2818 25.5635 22.3903C26.8274 20.4988 27.502 18.2749 27.502 16C27.502 14.4895 27.2045 12.9938 26.6264 11.5983C26.0484 10.2028 25.2012 8.9348 24.1331 7.86672C23.065 6.79865 21.797 5.9514 20.4015 5.37336C19.006 4.79532 17.5103 4.4978 15.9998 4.4978Z" fill="#FFAE28"/>
<path d="M16 0C12.8355 0 9.74207 0.938383 7.11088 2.69649C4.4797 4.45459 2.42894 6.95344 1.21793 9.87706C0.00693257 12.8007 -0.309921 16.0177 0.307443 19.1214C0.924806 22.2251 2.44866 25.0761 4.6863 27.3137C6.92394 29.5513 9.77486 31.0752 12.8786 31.6926C15.9823 32.3099 19.1993 31.9931 22.1229 30.7821C25.0466 29.5711 27.5454 27.5203 29.3035 24.8891C31.0616 22.2579 32 19.1645 32 16C32 11.7565 30.3143 7.68687 27.3137 4.68629C24.3131 1.68571 20.2435 0 16 0V0ZM16 27.2533C13.7743 27.2533 11.5986 26.5933 9.74799 25.3568C7.89739 24.1203 6.45502 22.3627 5.60328 20.3065C4.75154 18.2502 4.52869 15.9875 4.9629 13.8046C5.39711 11.6216 6.46889 9.6165 8.0427 8.04269C9.6165 6.46888 11.6217 5.39711 13.8046 4.96289C15.9875 4.52868 18.2502 4.75154 20.3065 5.60327C22.3627 6.45501 24.1203 7.89738 25.3568 9.74798C26.5933 11.5986 27.2533 13.7743 27.2533 16C27.2533 18.9846 26.0677 21.8469 23.9573 23.9573C21.8469 26.0677 18.9846 27.2533 16 27.2533Z" fill="#FFAE28"/>
</svg>`)

// wizStep is one row of the setup progress tracker.
type wizStep struct {
	N     int
	Title string
	State string // "done" | "current" | "todo"
}

// setupView / successView feed the two branded templates.
type setupView struct {
	Logo              template.HTML
	Host, User, Error string
	Steps             []wizStep
}
type successView struct {
	Logo  template.HTML
	Note  string
	Steps []wizStep
}

// steps returns the full setup checklist with the given step (1-based) marked
// current, earlier ones done, later ones to-do. The same list renders on both
// pages so the user always sees the whole journey and what remains.
func steps(current int) []wizStep {
	titles := []string{
		"Install the Rise bridge",
		"Enter your proxy credentials",
		"Restart Claude or start a new chat",
		"Approve the Metabase sign-in",
	}
	out := make([]wizStep, len(titles))
	for i, t := range titles {
		state := "todo"
		switch {
		case i+1 < current:
			state = "done"
		case i+1 == current:
			state = "current"
		}
		out[i] = wizStep{N: i + 1, Title: t, State: state}
	}
	return out
}

// brandHead is the shared <head> + Rise Design System styling for both pages.
const brandHead = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Rise MCP Bridge — setup</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Open+Sans:wght@400;600;700&amp;family=Source+Serif+4:wght@600;700&amp;display=swap" rel="stylesheet">
<style>
*{box-sizing:border-box}
body{margin:0;padding:40px 16px;background:#F2F5FB;color:#2B333A;font-family:'Open Sans',Verdana,Arial,sans-serif;font-size:16px;line-height:24px}
.card{max-width:520px;margin:0 auto;background:#fff;border-radius:14px;padding:32px 36px;box-shadow:0 10px 30px rgba(9,36,124,.10),0 1px 3px rgba(9,36,124,.08)}
.logo{margin-bottom:22px}
.logo svg{height:30px;width:auto;display:block}
h1{font-family:'Source Serif 4','Source Serif Pro',Georgia,serif;font-weight:700;font-size:32px;line-height:40px;color:#0F151B;margin:0 0 8px}
.lead{font-size:16px;line-height:24px;color:#555C61;margin:0 0 4px}
label{display:block;margin:18px 0 6px;font-weight:600;font-size:14px;line-height:20px;color:#2B333A}
input{width:100%;padding:11px 12px;font-size:16px;font-family:inherit;color:#0F151B;border:1px solid #CBCFD9;border-radius:8px}
input:focus{outline:none;border-color:#555DF2;box-shadow:0 0 0 3px rgba(85,93,242,.18)}
.pwwrap{position:relative}
.pwwrap input{padding-right:80px}
.pwtoggle{position:absolute;right:6px;top:50%;transform:translateY(-50%);margin:0;padding:7px 12px;font-size:12px;font-weight:600;font-family:inherit;background:#F2F5FB;color:#2B333A;border:1px solid #CBCFD9;border-radius:6px;cursor:pointer}
.pwtoggle:hover{background:#E1E6EF}
.primary{margin-top:24px;width:100%;padding:13px 18px;font-size:16px;font-weight:600;font-family:inherit;border:0;border-radius:8px;background:#555DF2;color:#fff;cursor:pointer}
.primary:hover{background:#4D54DA}
.primary:active{background:#444AC2}
.err{font-size:14px;line-height:20px;font-weight:600;color:#B30000;margin:18px 0 0;padding:11px 13px;border:1px solid #F08080;border-radius:8px;background:#FDECEC}
.note{font-size:14px;line-height:20px;color:#2B333A;margin:18px 0 0;padding:11px 13px;border:1px solid #FFE699;border-radius:8px;background:#FFF0C1}
.div{border:0;border-top:1px solid #E1E6EF;margin:26px 0 18px}
.overline{font-size:12px;line-height:16px;font-weight:700;letter-spacing:.5px;text-transform:uppercase;color:#09247C;margin:0 0 8px}
.steps{list-style:none;margin:0;padding:0}
.step{display:flex;align-items:flex-start;gap:12px;padding:9px 0}
.badge{flex:0 0 auto;width:26px;height:26px;border-radius:50%;display:flex;align-items:center;justify-content:center;font-weight:700;font-size:13px}
.steptitle{font-size:16px;line-height:26px;color:#6A7075}
.step.todo .badge{background:#E1E6EF;color:#AAADB0}
.step.current .badge{background:#555DF2;color:#fff}
.step.current .steptitle{color:#0F151B;font-weight:700}
.step.done .badge{background:#49D186;color:#fff}
.step.done .steptitle{color:#555C61}
.check{display:inline-flex;align-items:center;justify-content:center;width:46px;height:46px;border-radius:50%;background:#C8F1DA;color:#42BD79;font-size:24px;font-weight:700;margin-bottom:14px}
</style></head>`

// stepsBlock is the shared progress-tracker markup, ranged over .Steps.
const stepsBlock = `<ol class="steps">{{range .Steps}}<li class="step {{.State}}"><span class="badge">{{if eq .State "done"}}&#10003;{{else}}{{.N}}{{end}}</span><span class="steptitle">{{.Title}}</span></li>{{end}}</ol>`

var setupPage = template.Must(template.New("setup").Parse(brandHead + `<body>
<div class="card">
<div class="logo">{{.Logo}}</div>
<h1>Connect to Rise data</h1>
<p class="lead">Enter the same proxy username and password your SmartProxy browser extension uses. They’re stored only on this computer.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="POST" action="/save">
<label for="host">Proxy server</label><input id="host" name="host" value="{{.Host}}">
<label for="user">Username</label><input id="user" name="user" value="{{.User}}" autofocus>
<label for="pass">Password</label>
<div class="pwwrap"><input id="pass" name="pass" type="password"><button type="button" id="pwtoggle" class="pwtoggle">Show</button></div>
<button type="submit" class="primary">Save &amp; connect</button>
</form>
<hr class="div">
<p class="overline">Setup steps</p>
` + stepsBlock + `
</div>
<script>
(function(){
  var b=document.getElementById('pwtoggle'), p=document.getElementById('pass');
  if(!b||!p)return;
  b.addEventListener('click',function(){
    var show=p.type==='password';
    p.type=show?'text':'password';
    b.textContent=show?'Hide':'Show';
  });
})();
</script>
</body></html>`))

var successPage = template.Must(template.New("success").Parse(brandHead + `<body>
<div class="card">
<div class="logo">{{.Logo}}</div>
<span class="check">&#10003;</span>
<h1>You’re connected</h1>
<p class="lead">Your proxy credentials checked out and the Rise bridge is set up. Two things left:</p>
<hr class="div">
<p class="overline">What’s next</p>
` + stepsBlock + `
{{if .Note}}<p class="note">{{.Note}}</p>{{end}}
<p class="lead" style="margin-top:20px">You can close this tab and return to Claude.</p>
</div>
</body></html>`))

func setupHandler(configDir, prefHost, prefUser string, done chan<- error) http.Handler {
	mux := http.NewServeMux()
	// Count proxy-auth rejections this session. A single failure is usually a typo;
	// repeated failures suggest a real account problem, so we escalate to "contact
	// IT". The local setup server is single-user on loopback, so no locking needed.
	var failedAttempts int
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_ = setupPage.Execute(w, setupView{Logo: riseLogo, Host: prefHost, User: prefUser, Steps: steps(2)})
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

		// Re-render the form (server stays alive) with an error, so the user can fix
		// and resubmit without restarting the app. Host/User are kept; password is not.
		reject := func(msg string) {
			w.WriteHeader(http.StatusOK)
			_ = setupPage.Execute(w, setupView{Logo: riseLogo, Host: host, User: user, Error: msg, Steps: steps(2)})
		}

		if host == "" || user == "" || pass == "" {
			reject("All three fields are required.")
			return
		}

		// Verify the credentials actually work against the proxy before saving, so a
		// typo'd password is caught here rather than as a silent "won't connect" later.
		cfg := &SharedConfig{ProxyHost: host, ProxyUser: user, ProxyPass: pass}
		ok, note := verifyProxyCreds(cfg)
		if !ok {
			failedAttempts++
			msg := note
			if msg == "" {
				msg = "The proxy rejected that username or password. They’re the same credentials your SmartProxy browser extension uses — double-check them and try again."
			}
			if failedAttempts >= 2 {
				msg += " Still no luck? Contact your IT team — your proxy account may be locked or its password may have changed."
			}
			reject(msg)
			return
		}

		if err := saveShared(configDir, cfg); err != nil {
			reject("Could not save config: " + err.Error())
			done <- err
			return
		}

		_ = successPage.Execute(w, successView{Logo: riseLogo, Note: note, Steps: steps(3)})
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
