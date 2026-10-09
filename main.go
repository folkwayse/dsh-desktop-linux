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
	profile     string
	port        int
	width       int
	height      int
	dshBin      string
	url         string
	debug       bool
	noDmabuf    bool
	safeRender  bool
	opaqueMenus bool
	verbose     bool
	selftest    time.Duration
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
	fs.BoolVar(&cfg.opaqueMenus, "opaque-menus", true, "make the client's translucent menus opaque, so their labels stay readable (pass -opaque-menus=false to keep the upstream translucency)")
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
	early     map[string]any // first-paint colour sample, for the self-test
}

func run(cfg config) int {
	w := webview.New(cfg.debug)
	defer w.Destroy()

	w.SetTitle(appName)
	w.SetSize(cfg.width, cfg.height, webview.HintNone)

	a := &app{cfg: cfg, w: w, log: newTail(8 << 10), probe: make(chan string, 1)}
	a.alive.Store(true)

	// Schedule the probe before the first paint. It fires once, `selftest` after
	// startup, so it samples whatever is on screen by then: the boot screen when
	// the host is still starting (or never starts), the app once it has loaded.
	// Scheduling it from navigate() alone would make the boot screen unmeasurable.
	w.SetHtml(loadingPage(cfg))
	a.scheduleProbe()

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
` + readableSurfacesJS(cfg) + `
})();`)

	if cfg.selftest > 0 {
		// The early sample is reported through its own binding because navigating
		// to the app replaces the document, which would wipe any window global.
		if err := w.Bind("__dshEarlyPaint", func(payload string) {
			var m map[string]any
			if json.Unmarshal([]byte(payload), &m) == nil {
				a.mu.Lock()
				a.early = m
				a.mu.Unlock()
			}
		}); err != nil {
			fmt.Fprintln(os.Stderr, "dsh-desktop: cannot bind early-paint probe:", err)
			return 2
		}
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
			// Sample the canvas almost immediately: if WebKit paints white before
			// the boot screen's stylesheet lands, this is where it shows up.
			time.Sleep(250 * time.Millisecond)
			a.w.Dispatch(func() {
				a.w.Eval(`(function(){
  function sample() {
    var cs = getComputedStyle(document.body);
    return { body: cs.backgroundColor, html: getComputedStyle(document.documentElement).backgroundColor, text: (document.body ? (document.body.innerText || '').slice(0, 30) : '') };
  }
  __dshEarlyPaint(JSON.stringify(sample()));
})();`)
			})
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
			// Open the agent-preset picker so the probe can inspect its surface.
			a.w.Dispatch(func() {
				a.w.Eval(`(function(){
  var nodes = document.querySelectorAll('button');
  for (var i = 0; i < nodes.length; i++) {
    var t = (nodes[i].textContent || '').trim();
    if (/^(Montir|Standard|Minimal|Ptc|Cordis)$/.test(t)) {
      nodes[i].click();
      window.__dshOpened = t;
      return;
    }
  }
  window.__dshOpened = 'no preset trigger found';
})();`)
			})
			time.Sleep(1200 * time.Millisecond)
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
    presetOptions: (function(){
      // Which agent presets the picker offers. The names come from the profile's
      // roster, so this is how the test proves the desktop window sees the same
      // presets as the CLI.
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
    nav: window.__dshNav || '',
    theme: (function(){
      // The theme tokens are not resolvable from :root, so sample what the
      // browser actually painted instead: aggregate every element's background
      // by painted area and keep the dominant colours.
      var area = {}, borders = {}, texts = {}, nodes = document.querySelectorAll('*');
      for (var i = 0; i < nodes.length; i++) {
        var el = nodes[i], r = el.getBoundingClientRect();
        var px = Math.max(0, r.width) * Math.max(0, r.height);
        var cs = getComputedStyle(el);
        var bg = cs.backgroundColor;
        if (bg && bg !== 'rgba(0, 0, 0, 0)' && px > 0) area[bg] = (area[bg] || 0) + px;
        var bw = parseFloat(cs.borderTopWidth) || 0;
        if (bw > 0 && px > 0) {
          var bc = cs.borderTopColor;
          if (bc && bc !== 'rgba(0, 0, 0, 0)') borders[bc] = (borders[bc] || 0) + r.width;
        }
        if (el.children.length === 0 && (el.textContent || '').trim()) {
          var col = cs.color;
          if (col) texts[col] = (texts[col] || 0) + 1;
        }
      }
      function top(m, n) {
        return Object.keys(m).sort(function(a, b) { return m[b] - m[a]; }).slice(0, n)
          .map(function(k) { return [k, Math.round(m[k])]; });
      }
      var cs2 = getComputedStyle(document.body);
      return {
        htmlBg: getComputedStyle(document.documentElement).backgroundColor,
        bodyBg: cs2.backgroundColor,
        bodyColor: cs2.color,
        backgrounds: top(area, 8),
        borders: top(borders, 5),
        textColors: top(texts, 5)
      };
    })(),
    diag: (function(){
      function findLeaf(text) {
        var all = document.querySelectorAll('*');
        for (var i = 0; i < all.length; i++) {
          if (all[i].children.length === 0 && (all[i].textContent || '').trim() === text) return all[i];
        }
        return null;
      }
      var out = { supportsBackdrop: CSS.supports('backdrop-filter','blur(40px)'), opened: window.__dshOpened || '' };
      var menus = [], cands = document.querySelectorAll('[role="menu"],[role="listbox"],[class*="material"],[class*="surface"],[class*="Surface"]');
      for (var mi = 0; mi < cands.length && menus.length < 5; mi++) {
        var el = cands[mi], cs3 = getComputedStyle(el), rr = el.getBoundingClientRect();
        if (rr.width < 40 || rr.height < 20) continue;
        var txt = (el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 60);
        menus.push({ tag: el.tagName, cls: String(el.className || '').slice(0, 46), role: el.getAttribute('role') || '',
                     bg: cs3.backgroundColor, bf: cs3.backdropFilter, color: cs3.color, z: cs3.zIndex,
                     w: Math.round(rr.width), h: Math.round(rr.height), text: txt });
      }
      out.menus = menus;
      // The readability fix is about the menu's *material* layer: the element
      // that carries the fill and the blur. Report its alpha explicitly so the
      // smoke test can assert the surface is fully opaque.
      var mat = document.querySelector('[class*="material"]');
      if (mat) {
        var mcs = getComputedStyle(mat);
        var m = /rgba?\(([^)]+)\)/.exec(mcs.backgroundColor);
        var parts = m ? m[1].split(',').map(function(x){ return parseFloat(x); }) : [];
        out.menuMaterial = {
          cls: String(mat.className || '').slice(0, 44),
          bg: mcs.backgroundColor,
          alpha: parts.length === 4 ? parts[3] : (parts.length === 3 ? 1 : null),
          backdropFilter: mcs.backdropFilter
        };
      } else {
        out.menuMaterial = null;
      }
      var cs2 = getComputedStyle(document.body);
      out.tokens = {
        menuFill: cs2.getPropertyValue('--dsw-menu-surface-fill').trim(),
        menuBf: cs2.getPropertyValue('--dsw-menu-backdrop-filter').trim(),
        overlay: cs2.getPropertyValue('--dsw-alias-bg-overlay').trim(),
        layer1: cs2.getPropertyValue('--dsw-alias-bg-layer-1').trim(),
        base: cs2.getPropertyValue('--dsw-alias-bg-base').trim()
      };
      return out;
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
		URL           string         `json:"url"`
		Title         string         `json:"title"`
		Ready         string         `json:"readyState"`
		HasRoot       bool           `json:"hasRoot"`
		Buttons       int            `json:"buttons"`
		BodyText      string         `json:"bodyText"`
		PresetOptions []string       `json:"presetOptions"`
		Nav           string         `json:"nav"`
		Diag          map[string]any `json:"diag"`
		Theme         map[string]any `json:"theme"`
		Errors        []string       `json:"errors"`
		RootKids      int            `json:"rootChildren"`
		StyleShee     int            `json:"stylesheets"`
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
		"diag":          got.Diag,
		"early":         a.early,
		"theme":         got.Theme,
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

// pageStyle dresses the boot and error screens in the client's own palette so the
// window does not flash a different colour before the app paints. These are the
// values the running client actually paints, measured from the live document
// rather than guessed:
//
//	base background   rgb(21, 21, 23)     #151517
//	raised surface    rgb(27, 27, 28)     #1b1b1c
//	stronger border   rgb(44, 44, 46)     #2c2c2e
//	primary label     rgb(249, 250, 251)  #f9fafb
//	secondary label   rgb(173, 178, 184)  #adb2b8
//	tertiary label    rgb(129, 133, 140)  #81858c
//
// The accent stays DeepSeek blue. The client's theme is its own setting rather
// than the desktop's, so these match the dark theme it ships with.
const pageStyle = `
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  html, body { height: 100%%; margin: 0; background: #151517; }
  body {
    display: flex; align-items: center; justify-content: center;
    background: #151517; color: #f9fafb;
    font: 14px/1.55 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    -webkit-user-select: none; user-select: none;
  }
  .card { width: min(560px, 84vw); text-align: center; }
  h1 { font-size: 17px; font-weight: 600; margin: 0 0 6px; letter-spacing: .01em; }
  p { margin: 0; color: #adb2b8; }
  strong { color: #f9fafb; font-weight: 600; }
  .spinner {
    width: 26px; height: 26px; margin: 0 auto 20px;
    border: 2px solid #2c2c2e; border-top-color: #4d6bfe; border-radius: 50%%;
    animation: spin .8s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  .err { border-color: #2c2c2e; border-top-color: #e5484d; animation: none; }
  pre {
    margin: 20px 0 0; padding: 14px; max-height: 34vh; overflow: auto; text-align: left;
    background: #1b1b1c; border: 1px solid #2c2c2e; border-radius: 8px;
    font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #adb2b8; white-space: pre-wrap; word-break: break-word;
    -webkit-user-select: text; user-select: text;
  }
  .hint { margin-top: 18px; font-size: 12.5px; color: #81858c; }
  code { background: #1b1b1c; border: 1px solid #2c2c2e; border-radius: 4px; padding: 1px 5px; font-size: 12px; color: #adb2b8; }
`

// pageHead carries the colour-scheme hint that stops WebKit painting a white
// canvas in the moment before the stylesheet above applies.
const pageHead = `<meta charset="utf-8"><meta name="color-scheme" content="dark">`

// readableSurfacesCSS makes the client's translucent menus opaque.
//
// The client styles menu surfaces as `--dsw-menu-surface-fill: #43454a73` -- 45%
// opaque -- and relies on `backdrop-filter: blur(40px)` for contrast. That works
// on Chromium, where the blur composites the page behind the menu. On the
// WebKitGTK path this app uses, the blur does not composite, so the menu renders
// as a see-through panel and its labels are unreadable against the content
// underneath.
//
// The replacement is that same fill composited over the base background:
//
//	#43454a at 45% over #151517  ->  #2a2b2e
//
// which is what the menu is meant to look like once the blur has done its work.
// The fill no longer depends on the blur, so the blur is switched off with it.
const readableSurfacesCSS = `
:root, body {
  --dsw-menu-surface-fill: #2a2b2e !important;
  --dsw-menu-backdrop-filter: none !important;
}
`

// readableSurfacesJS returns the script that installs readableSurfacesCSS into
// the document. It runs from Init, so it is in place before the client's own
// stylesheet and survives the navigation from the boot screen to the app.
func readableSurfacesJS(cfg config) string {
	if !cfg.opaqueMenus {
		return ""
	}
	return `  (function(){
    var css = ` + strconv.Quote(readableSurfacesCSS) + `;
    function install() {
      if (document.getElementById('dsh-desktop-readability')) return;
      var el = document.createElement('style');
      el.id = 'dsh-desktop-readability';
      el.textContent = css;
      (document.head || document.documentElement).appendChild(el);
    }
    if (document.head) install();
    else document.addEventListener('DOMContentLoaded', install);
  })();`
}

func loadingPage(cfg config) string {
	return `<!doctype html>
<html><head>` + pageHead + `<title>` + appName + `</title>
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
<html><head>` + pageHead + `<title>` + appName + ` — error</title>
<style>` + fmt.Sprintf(pageStyle) + `</style></head>
<body>` + body + `</body></html>`
}
