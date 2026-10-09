// Command dsh-desktop is a small native desktop shell for DeepSeek Harness.
//
// It boots a Harness profile (`dsh web`) as a child process, reads the
// authenticated loopback URL the host prints on stdout, and loads that URL in a
// native WebKitGTK window. There is no Electron, no bundled Chromium, and no
// second Node runtime: the only heavyweight dependency is the WebKit engine
// that a GTK desktop already ships.
//
// The whole thing is one static-ish Go binary of a few megabytes.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	webview "github.com/webview/webview_go"
)

const (
	appName       = "DeepSeek Harness"
	shutdownGrace = 6 * time.Second
	killGrace     = 2 * time.Second

	// defaultPort sits above 20000 but below the Linux ephemeral range
	// (32768-60999), so the OS is unlikely to hand the same port to something
	// else while the window is open. A stable port also means the URL survives
	// restarts, which makes it bookmarkable and reusable via -url.
	defaultPort = 21777

	// If the preferred port is taken, walk upward this many ports before giving
	// up. Every candidate stays above 20000.
	portScanLimit = 64

	// A probe-then-bind race can still lose the port to another process in the
	// milliseconds between the two, so a failed boot is retried from the next
	// free port rather than shown to the user as an error.
	hostStartAttempts = 3
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "0.1.0"

// The host prints exactly one line of this shape:
//
//	dsh web: http://127.0.0.1:37907/?token=cQVcPsNLazo9whuwBREC7PlarY_4XQGkpyDvJYN2cws
var urlPattern = regexp.MustCompile(`https?://(?:127\.0\.0\.1|localhost):\d+/\?token=[A-Za-z0-9._~-]+`)

type config struct {
	profile    string
	port       int
	width      int
	height     int
	dshBin     string
	url        string
	debug      bool
	noDmabuf   bool
	safeRender bool
	verbose    bool
	selftest   time.Duration
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "dsh-desktop:", err)
		os.Exit(2)
	}

	// WebKit reads these once, at engine init, so they must be set before the
	// first window exists. Both are workarounds for the fatal
	// "Could not create GBM EGL display: EGL_SUCCESS. Aborting..." crash and for
	// blank/garbled pages on compositing-hostile drivers. An existing value in
	// the environment always wins, so a user can force either behaviour.
	if cfg.noDmabuf {
		setDefaultEnv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}
	if cfg.safeRender {
		setDefaultEnv("WEBKIT_DISABLE_COMPOSITING_MODE", "1")
	}

	unlock, err := acquireLock(cfg.profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dsh-desktop:", err)
		os.Exit(1)
	}
	defer unlock()

	os.Exit(run(cfg))
}

func parseFlags(args []string) (config, error) {
	var (
		cfg         config
		showVersion bool
	)
	fs := flag.NewFlagSet("dsh-desktop", flag.ContinueOnError)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "%s — native desktop window %s\n\n", appName, version)
		fmt.Fprintf(out, "Usage: dsh-desktop [options]\n\n")
		fmt.Fprintf(out, "Boots a Harness profile in a native WebKitGTK window.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&cfg.profile, "profile", "web", "Harness profile to boot (the profile that owns your agent presets)")
	fs.IntVar(&cfg.port, "port", defaultPort, "loopback port for the host; if it is taken the app moves up to the next free one (0 lets the OS choose)")
	fs.IntVar(&cfg.width, "width", 1280, "initial window width")
	fs.IntVar(&cfg.height, "height", 840, "initial window height")
	fs.StringVar(&cfg.dshBin, "dsh", "", "path to the dsh executable (default: $DSH_BIN, then PATH, then node + installed CLI)")
	fs.StringVar(&cfg.url, "url", "", "load this URL directly instead of booting a host (testing / attaching)")
	fs.BoolVar(&cfg.debug, "debug", false, "enable the WebKit inspector")
	fs.BoolVar(&cfg.noDmabuf, "no-dmabuf", true, "disable WebKit's DMA-BUF renderer (prevents the fatal \"Could not create GBM EGL display\" abort on some drivers); pass -no-dmabuf=false to re-enable")
	fs.BoolVar(&cfg.safeRender, "safe-render", false, "additionally disable WebKit compositing mode, for drivers that still render blank or garbled")
	fs.BoolVar(&cfg.verbose, "verbose", false, "echo the host's output to stderr")
	fs.DurationVar(&cfg.selftest, "selftest", 0, "load the app, inspect the rendered page, print a JSON report and exit (e.g. 20s)")
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if showVersion {
		fmt.Printf("dsh-desktop %s\n", version)
		return cfg, flag.ErrHelp
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if cfg.width < 320 || cfg.height < 240 {
		return cfg, errors.New("window must be at least 320x240")
	}
	if cfg.port < 0 || cfg.port > 65535 {
		return cfg, errors.New("port must be between 0 and 65535")
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// single instance, per profile
// ---------------------------------------------------------------------------

func acquireLock(profile string) (func(), error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, "dsh-desktop-"+sanitize(profile)+".lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, nil // a missing lock is not worth refusing to start over
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another dsh-desktop window is already open for profile %q", profile)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// setDefaultEnv sets an environment variable only when the caller has not
// already chosen a value for it.
func setDefaultEnv(key, value string) {
	if _, ok := os.LookupEnv(key); !ok {
		os.Setenv(key, value)
	}
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// ---------------------------------------------------------------------------
// app
// ---------------------------------------------------------------------------

type app struct {
	cfg   config
	w     webview.WebView
	alive atomic.Bool
	log   *tail

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}

	probeOnce sync.Once
	probe     chan string
	report    string
}

func run(cfg config) int {
	w := webview.New(cfg.debug)
	defer w.Destroy()

	w.SetTitle(appName)
	w.SetSize(cfg.width, cfg.height, webview.HintNone)
	w.SetHtml(loadingPage(cfg))

	a := &app{cfg: cfg, w: w, log: newTail(8 << 10), probe: make(chan string, 1)}
	a.alive.Store(true)

	// Collect page-level failures so the self-test can report a real reason
	// instead of just "the root element is missing".
	w.Init(`(function(){
  window.__dshErrors = [];
  window.addEventListener('error', function(e){
    window.__dshErrors.push(String((e && (e.message || e.error)) || 'error'));
  }, true);
  window.addEventListener('unhandledrejection', function(e){
    window.__dshErrors.push('unhandled rejection: ' + String(e && e.reason));
  });
})();`)

	if cfg.selftest > 0 {
		if err := w.Bind("__dshSelfTest", func(payload string) {
			select {
			case a.probe <- payload:
			default:
			}
			w.Terminate()
		}); err != nil {
			fmt.Fprintln(os.Stderr, "dsh-desktop: cannot bind self-test probe:", err)
			return 2
		}
	}

	if cfg.url != "" {
		a.navigate(cfg.url)
	} else {
		go a.startHost()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		w.Terminate()
	}()

	w.Run() // blocks until the window closes

	a.alive.Store(false)
	a.stopHost()

	if cfg.selftest > 0 {
		select {
		case a.report = <-a.probe:
		case <-time.After(5 * time.Second):
		}
		return a.printSelfTest()
	}
	return 0
}

// newSessionSettle is how long the client gets to swap from whatever view it
// restored to the empty-session view that carries the agent-preset picker.
const newSessionSettle = 3 * time.Second

// scheduleProbe inspects the rendered document once, after the app has had time
// to boot. It is the only way to prove WebKit actually executed the client,
// which no amount of "the window exists" checking can show.
//
// The client restores the route it was last on, which may be any settings page.
// The agent-preset picker only exists on the empty-session view, so the probe
// navigates there first: otherwise the preset check would pass or fail depending
// on where the user happened to leave the app.
func (a *app) scheduleProbe() {
	if a.cfg.selftest <= 0 {
		return
	}
	a.probeOnce.Do(func() {
		go func() {
			time.Sleep(a.cfg.selftest)
			a.w.Dispatch(func() {
				a.w.Eval(`(function(){
  // The empty-session view is the only one with the agent-preset picker. It is
  // identifiable by its composer placeholder, so if that is already on screen
  // the route was restored correctly and there is nothing to click.
  if (document.body && document.body.innerText.indexOf('Describe what you want to build') !== -1) {
    window.__dshNav = 'already on the empty-session view';
    return;
  }
  var nodes = document.querySelectorAll('button, a, [role="button"], [role="menuitem"], [role="tab"]');
  for (var i = 0; i < nodes.length; i++) {
    if ((nodes[i].textContent || '').trim() === 'New Session') {
      nodes[i].click();
      window.__dshNav = 'clicked New Session';
      return;
    }
  }
  window.__dshNav = 'no New Session control found';
})();`)
			})
			time.Sleep(newSessionSettle)
			a.w.Dispatch(func() {
				a.w.Eval(`(function(){
  var root = document.querySelector('#root, #app, [data-dsh-root], main');
  var info = {
    url: location.href,
    title: document.title,
    readyState: document.readyState,
    hasRoot: !!root,
    rootChildren: root ? root.children.length : 0,
    buttons: document.querySelectorAll('button').length,
    stylesheets: document.styleSheets.length,
    scripts: document.scripts.length,
    bodyText: (document.body ? (document.body.innerText || document.body.textContent || '') : '').replace(/\s+/g, ' ').trim().slice(0, 500),
    nav: window.__dshNav || '',
    presetOptions: (function(){
      var names = ['Montir','Standard','Ptc','Minimal','Cordis'];
      var seen = [];
      var nodes = document.querySelectorAll('button, [role="option"], [role="menuitem"], li, span, div');
      for (var i = 0; i < nodes.length && seen.length < 12; i++) {
        var el = nodes[i];
        if (el.children.length > 0) continue;
        var t = (el.textContent || '').trim();
        if (names.indexOf(t) !== -1 && seen.indexOf(t) === -1) seen.push(t);
      }
      return seen;
    })(),
    errors: (window.__dshErrors || []).slice(0, 10)
  };
  __dshSelfTest(JSON.stringify(info));
})();`)
			})
		}()
	})
}

func (a *app) printSelfTest() int {
	if a.report == "" {
		fmt.Println(`{"ok":false,"reason":"the page never reported back — the window opened but nothing executed"}`)
		return 1
	}
	var got struct {
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		Ready         string   `json:"readyState"`
		HasRoot       bool     `json:"hasRoot"`
		Buttons       int      `json:"buttons"`
		BodyText      string   `json:"bodyText"`
		PresetOptions []string `json:"presetOptions"`
		Nav           string   `json:"nav"`
		Errors        []string `json:"errors"`
		RootKids      int      `json:"rootChildren"`
		StyleShee     int      `json:"stylesheets"`
	}
	if err := json.Unmarshal([]byte(a.report), &got); err != nil {
		fmt.Printf("{\"ok\":false,\"reason\":\"unreadable probe payload\",\"raw\":%q}\n", a.report)
		return 1
	}
	loaded := got.HasRoot && got.BodyText != "" && len(got.Errors) == 0
	out := map[string]any{
		"ok":            loaded,
		"url":           got.URL,
		"title":         got.Title,
		"readyState":    got.Ready,
		"hasRoot":       got.HasRoot,
		"rootChildren":  got.RootKids,
		"buttons":       got.Buttons,
		"stylesheets":   got.StyleShee,
		"bodyText":      got.BodyText,
		"presetOptions": got.PresetOptions,
		"nav":           got.Nav,
		"pageErrors":    got.Errors,
		"hostLogTail":   strings.TrimSpace(a.log.String()),
		"profile":       a.cfg.profile,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return 1
	}
	if !loaded {
		return 1
	}
	return 0
}

// pickPort returns a loopback port that is free right now, starting at preferred
// and walking upward. A preferred value of 0 is passed through unchanged so the
// OS can choose. Every candidate stays above the 20000 mark that the port range
// is meant to live in.
func pickPort(preferred int) (int, error) {
	if preferred <= 0 {
		return 0, nil
	}
	for port := preferred; port < preferred+portScanLimit && port <= 65535; port++ {
		if portFree(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("ports %d-%d are all in use", preferred, preferred+portScanLimit-1)
}

// portFree reports whether the loopback port can be bound right now. The listener
// is closed immediately: this is only a probe, and the child process is what
// actually holds the port. That gap is small but real, which is why the caller
// retries rather than trusting the answer.
func portFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// startHost spawns `dsh web` and watches its stdout for the authenticated URL.
//
// The host refuses to start at all when its port is taken (it exits with
// EADDRINUSE rather than picking another), so a lost race is recovered here by
// retrying from the next free port.
func (a *app) startHost() {
	bin, prefix, err := resolveDsh(a.cfg.dshBin)
	if err != nil {
		a.showError("Cannot find the dsh command", err.Error(), "")
		return
	}

	preferred := a.cfg.port
	for attempt := 1; ; attempt++ {
		port, err := pickPort(preferred)
		if err != nil {
			a.showError("No free port for the host", err.Error(), a.log.String())
			return
		}

		published, err := a.runHost(bin, prefix, port)
		if published {
			return
		}
		// Only a port collision is worth retrying; any other failure would just
		// repeat identically and should reach the user immediately.
		stolen := strings.Contains(a.log.String(), "EADDRINUSE")
		if attempt >= hostStartAttempts || !stolen {
			a.showError("DeepSeek Harness did not start", err.Error(), a.log.String())
			return
		}
		fmt.Fprintf(a.log, "port %d was taken before the host could bind it; retrying\n", port)
		preferred = port + 1
	}
}

// runHost performs one boot attempt on a specific port. It reports whether the
// host published its URL, so the caller can decide between success, a retry, and
// an error.
func (a *app) runHost(bin string, prefix []string, port int) (bool, error) {
	args := append(append([]string{}, prefix...), "web",
		"--port", strconv.Itoa(port), "--no-open")

	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "DSH_TELEMETRY_DISABLED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group: one signal reaches every child
	if a.cfg.verbose {
		cmd.Stderr = io.MultiWriter(a.log, os.Stderr)
	} else {
		cmd.Stderr = a.log
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("cannot capture the host's output: %w", err)
	}

	done := make(chan struct{})
	a.mu.Lock()
	a.cmd, a.done = cmd, done
	a.mu.Unlock()

	if err := cmd.Start(); err != nil {
		close(done)
		return false, fmt.Errorf("cannot start the host: %w", err)
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	found := false
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(a.log, line)
		if a.cfg.verbose {
			fmt.Fprintln(os.Stderr, "[dsh] "+line)
		}
		if found {
			continue
		}
		if u := urlPattern.FindString(line); u != "" {
			found = true
			a.navigate(u)
		}
	}

	waitErr := cmd.Wait()
	close(done)

	if found {
		return true, nil
	}
	if waitErr != nil {
		return false, fmt.Errorf("the host exited: %w", waitErr)
	}
	return false, errors.New("the host exited before it published a URL")
}

// stopHost terminates the whole host process group, escalating if it lingers.
func (a *app) stopHost() {
	a.mu.Lock()
	cmd, done := a.cmd, a.done
	a.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	select {
	case <-done:
		return
	case <-time.After(shutdownGrace):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(killGrace):
	}
}

func (a *app) navigate(u string) {
	if !a.alive.Load() {
		return
	}
	a.w.Dispatch(func() {
		if !a.alive.Load() {
			return
		}
		a.w.SetTitle(fmt.Sprintf("%s — %s", appName, a.cfg.profile))
		a.w.Navigate(u)
	})
	a.scheduleProbe()
}

func (a *app) showError(title, detail, tail string) {
	if !a.alive.Load() {
		return
	}
	a.w.Dispatch(func() {
		if !a.alive.Load() {
			return
		}
		a.w.SetTitle(appName + " — error")
		a.w.SetHtml(errorPage(title, detail, tail))
	})
}

// ---------------------------------------------------------------------------
// resolving the dsh executable
// ---------------------------------------------------------------------------

func resolveDsh(explicit string) (string, []string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", nil, fmt.Errorf("--dsh %q: %w", explicit, err)
		}
		return explicit, nil, nil
	}
	if v := os.Getenv("DSH_BIN"); v != "" {
		return v, nil, nil
	}
	if p, err := exec.LookPath("dsh"); err == nil {
		return p, nil, nil
	}
	// Last resort: drive the installed CLI through node directly. The CLI entry
	// point is the same file the npm bin shim executes.
	node, err := exec.LookPath("node")
	if err != nil {
		return "", nil, errors.New("neither `dsh` nor `node` is on PATH — install the CLI with `npm i -g @deepseek-ai/dsh`, or pass --dsh")
	}
	for _, c := range libCandidates() {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return node, []string{c}, nil
		}
	}
	return "", nil, errors.New("found node but not the dsh entry point — pass --dsh <path to dsh>")
}

func libCandidates() []string {
	out := []string{
		"/usr/local/lib/node_modules/@deepseek-ai/dsh/lib/bin.js",
		"/usr/lib/node_modules/@deepseek-ai/dsh/lib/bin.js",
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	for _, pat := range []string{
		".nvm/versions/node/*/lib/node_modules/@deepseek-ai/dsh/lib/bin.js",
		".config/nvm/versions/node/*/lib/node_modules/@deepseek-ai/dsh/lib/bin.js",
		".local/share/pnpm/global/*/node_modules/@deepseek-ai/dsh/lib/bin.js",
		".bun/install/global/node_modules/@deepseek-ai/dsh/lib/bin.js",
	} {
		if m, _ := filepath.Glob(filepath.Join(home, pat)); len(m) > 0 {
			out = append(out, m...)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// captured host output
// ---------------------------------------------------------------------------

// tail keeps the last max bytes written to it, so a failure page can quote the
// host's own diagnostics instead of a generic message.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTail(max int) *tail { return &tail{max: max} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// ---------------------------------------------------------------------------
// pages
// ---------------------------------------------------------------------------

const pageStyle = `
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  html, body { height: 100%%; margin: 0; }
  body {
    display: flex; align-items: center; justify-content: center;
    background: #0b0d10; color: #e6e8eb;
    font: 14px/1.55 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    -webkit-user-select: none; user-select: none;
  }
  .card { width: min(560px, 84vw); text-align: center; }
  h1 { font-size: 17px; font-weight: 600; margin: 0 0 6px; letter-spacing: .01em; }
  p { margin: 0; color: #8b929c; }
  .spinner {
    width: 26px; height: 26px; margin: 0 auto 20px;
    border: 2px solid #24282e; border-top-color: #4d6bfe; border-radius: 50%%;
    animation: spin .8s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  .err { border-top-color: #ff6b6b; animation: none; border-color: #3a2020; border-top-color: #ff6b6b; }
  pre {
    margin: 20px 0 0; padding: 14px; max-height: 34vh; overflow: auto; text-align: left;
    background: #14171b; border: 1px solid #23272d; border-radius: 8px;
    font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #9aa1ab; white-space: pre-wrap; word-break: break-word;
    -webkit-user-select: text; user-select: text;
  }
  .hint { margin-top: 18px; font-size: 12.5px; color: #6b727c; }
  code { background: #14171b; border: 1px solid #23272d; border-radius: 4px; padding: 1px 5px; font-size: 12px; }
`

func loadingPage(cfg config) string {
	return `<!doctype html>
<html><head><meta charset="utf-8"><title>` + appName + `</title>
<style>` + fmt.Sprintf(pageStyle) + `</style></head>
<body><div class="card">
  <div class="spinner"></div>
  <h1>Starting ` + appName + `</h1>
  <p>Booting profile <strong>` + html.EscapeString(cfg.profile) + `</strong>…</p>
  <div class="hint">First boot composes the plugin graph, so it can take a few seconds.</div>
</div></body></html>`
}

func errorPage(title, detail, output string) string {
	body := `<div class="card">
  <div class="spinner err"></div>
  <h1>` + html.EscapeString(title) + `</h1>
  <p>` + html.EscapeString(detail) + `</p>`
	if out := strings.TrimSpace(output); out != "" {
		body += `<pre>` + html.EscapeString(out) + `</pre>`
	}
	body += `<div class="hint">Check that the CLI runs: <code>dsh web</code>. Override the binary with <code>--dsh &lt;path&gt;</code>.</div>
</div>`
	return `<!doctype html>
<html><head><meta charset="utf-8"><title>` + appName + ` — error</title>
<style>` + fmt.Sprintf(pageStyle) + `</style></head>
<body>` + body + `</body></html>`
}
