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

// The portal numbers its colour-scheme reply: <uint32 N>.
var portalUint32 = regexp.MustCompile(`uint32\s+(\d+)`)

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

	// updateCheck is the boot-time check for a newer release. The two
	// commands below run it in the foreground instead, and exit.
	updateCheck bool
	checkUpdate bool
	update      bool
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

	// -check-update and -update are ordinary commands: no window, no profile,
	// no single-instance lock, so an update works while a window is open.
	if cfg.checkUpdate || cfg.update {
		os.Exit(runUpdateCommand(cfg))
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
	fs.BoolVar(&cfg.updateCheck, "update-check", os.Getenv(updateEnvOpt) == "", "check GitHub for a newer release after boot and show a banner when there is one (env "+updateEnvOpt+"=1 disables it)")
	fs.BoolVar(&cfg.checkUpdate, "check-update", false, "print the newest release and whether it is newer than this build, then exit")
	fs.BoolVar(&cfg.update, "update", false, "download the newest release and replace this binary, then exit (the previous binary is kept as dsh-desktop.backup)")
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.checkUpdate && cfg.update {
		return cfg, errors.New("-check-update and -update are separate commands; -update already reports what it installs")
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

	// notice is the banner text for a newer release, empty until the boot-time
	// check finds one. It is read from the page through a binding, so it is
	// stored atomically rather than behind the mutex the host uses.
	notice atomic.Value

	// themeStop ends the desktop's colour-scheme listener. It is called when
	// the window closes so the helper does not outlive the app.
	themeStop atomic.Value // func()
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
  window.__dshResourceErrors = [];
  window.addEventListener('error', function(e){
    // A failed image or attachment is not a page error: it has no message and
    // no source location, only a target element. Keep the two apart, or the
    // report drowns in unrelated 404s.
    if (e && e.target && e.target !== window && !e.message) {
      window.__dshResourceErrors.push(String(e.target.tagName || 'resource'));
      return;
    }
    var where = (e && e.filename) ? ' @ ' + e.filename + ':' + e.lineno : '';
    window.__dshErrors.push(String((e && (e.message || e.error)) || 'error') + where);
  }, true);
  window.addEventListener('unhandledrejection', function(e){
    window.__dshErrors.push('unhandled rejection: ' + String(e && e.reason));
  });
` + shellCSSJS(cfg) + `
` + schemeFollowJS() + `
` + updateBannerJS() + `
})();`)

	// The banner renders itself in every document and asks Go what to say, so
	// the answer can arrive long after the page has loaded.
	if err := w.Bind("__dshUpdateNotice", a.updateNotice); err != nil {
		fmt.Fprintln(os.Stderr, "dsh-desktop: cannot bind the update banner:", err)
	}

	if cfg.updateCheck && cfg.selftest == 0 {
		go a.checkForUpdateOnBoot()
	}

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

	go a.watchSystemTheme()

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
	if stop, ok := a.themeStop.Load().(func()); ok && stop != nil {
		stop()
	}
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
  var text = document.body ? (document.body.innerText || '') : '';
  if (text.indexOf('Describe what you want to build') !== -1 || text.indexOf('\u63cf\u8ff0\u4f60\u60f3\u8981\u6784\u5efa\u7684\u5185\u5bb9') !== -1) {
    window.__dshNav = 'already on the empty-session view';
    return;
  }
  var nodes = document.querySelectorAll('button, a, [role="button"], [role="menuitem"], [role="tab"]');
  for (var i = 0; i < nodes.length; i++) {
    var t = (nodes[i].textContent || '').trim();
    if (t === 'New Session' || t === '\u65b0\u4f1a\u8bdd') {
      nodes[i].click();
      window.__dshNav = 'clicked New Session';
      return;
    }
  }
  window.__dshNav = 'no New Session control found';
})();`)
			})
			time.Sleep(newSessionSettle)
			// Open the workspace picker and measure the surface it paints. The
			// menu fill is the thing this app has to get right: a translucent
			// fill that does not composite leaves the labels unreadable, and a
			// fill from the wrong theme leaves them dark on dark.
			a.w.Dispatch(func() {
				a.w.Eval(`(function(){
  var nodes = document.querySelectorAll('button, [role="button"]');
  for (var i = 0; i < nodes.length; i++) {
    var label = nodes[i].getAttribute('aria-label') || '';
    if (label.indexOf('\u9009\u62e9\u5de5\u4f5c\u533a') !== -1 || /select workspace/i.test(label)) {
      nodes[i].click();
      window.__dshPicker = 'clicked';
      return;
    }
  }
  window.__dshPicker = 'no workspace trigger found';
})();`)
			})
			time.Sleep(1200 * time.Millisecond)
			a.w.Dispatch(func() {
				a.w.Eval(`(function(){
  var out = { opened: window.__dshPicker || '' };
  // The panel that carries the fill is the material layer inside the menu.
  var panels = document.querySelectorAll('[role="menu"]');
  for (var i = 0; i < panels.length; i++) {
    var mat = panels[i].querySelector('[class*="material"]');
    if (!mat) continue;
    var cs = getComputedStyle(mat);
    out.menuBg = cs.backgroundColor;
    out.menuBlur = cs.backdropFilter;
    var m = /rgba?\(([^)]+)\)/.exec(cs.backgroundColor);
    var parts = m ? m[1].split(',').map(function (x) { return parseFloat(x); }) : [];
    out.menuAlpha = parts.length === 4 ? parts[3] : (parts.length === 3 ? 1 : null);
    var item = panels[i].querySelector('[role="menuitem"], button');
    if (item) out.itemColor = getComputedStyle(item).color;
    out.menuText = (panels[i].textContent || '').trim().replace(/\s+/g, ' ').slice(0, 120);
    break;
  }
  out.dark = !!(document.body && document.body.hasAttribute('data-ds-dark-theme'));
  // What the engine itself reports. The client paints from this, and it is
  // what the desktop's colour scheme has to change for the app to follow.
  out.prefersDark = matchMedia('(prefers-color-scheme: dark)').matches;
  out.themeSource = document.documentElement.getAttribute('data-ds-theme-source');
  window.__dshMenu = out;
})();`)
			})
			// Open the agent-preset picker so the probe can inspect its surface.
			// Close the workspace menu first, so the picker's own entries are
			// the only ones on screen.
			a.w.Dispatch(func() {
				a.w.Eval(`(function(){
  try {
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  } catch (e) {}
  var nodes = document.querySelectorAll('button');
  for (var i = 0; i < nodes.length; i++) {
    var t = (nodes[i].textContent || '').trim();
    var label = nodes[i].getAttribute('aria-label') || '';
    if (/^(Montir|Standard|Minimal|Ptc|Cordis)$/.test(t) || t === '\u6807\u51c6\u6a21\u5f0f' || /Standard mode/i.test(label)) {
      nodes[i].click();
      window.__dshOpened = t || label;
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
      // Which agent presets the picker offers. The names come from the
      // profile's roster, so this is how the test proves the desktop window
      // sees the same presets as the CLI. They are read off the open picker
      // rather than matched against a fixed list, because the roster is the
      // user's, not the app's.
      var seen = [];
      var menus = document.querySelectorAll('[role="menu"]');
      var menu = menus[menus.length - 1];
      if (!menu) return seen;
      var items = menu.querySelectorAll('[role="menuitem"], [role="option"], button');
      for (var i = 0; i < items.length && seen.length < 12; i++) {
        // Each entry carries a title and a description; take the title.
        var title = '';
        var leaves = items[i].querySelectorAll('*');
        for (var j = 0; j < leaves.length; j++) {
          if (leaves[j].children.length > 0) continue;
          var lt = (leaves[j].textContent || '').trim().replace(/\s+/g, ' ');
          if (lt && lt.length <= 24) { title = lt; break; }
        }
        if (!title) {
          var whole = (items[i].textContent || '').trim().replace(/\s+/g, ' ');
          title = whole.slice(0, 24);
        }
        if (title && seen.indexOf(title) === -1) seen.push(title);
      }
      return seen;
    })(),
    nav: window.__dshNav || '',
    menu: window.__dshMenu || null,
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
    errors: (window.__dshErrors || []).slice(0, 10),
    resourceErrors: (window.__dshResourceErrors || []).slice(0, 10)
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
		Menu          map[string]any `json:"menu"`
		Diag          map[string]any `json:"diag"`
		Theme         map[string]any `json:"theme"`
		Errors        []string       `json:"errors"`
		ResourceErrs  []string       `json:"resourceErrors"`
		RootKids      int            `json:"rootChildren"`
		StyleShee     int            `json:"stylesheets"`
	}
	if err := json.Unmarshal([]byte(a.report), &got); err != nil {
		fmt.Printf("{\"ok\":false,\"reason\":\"unreadable probe payload\",\"raw\":%q}\n", a.report)
		return 1
	}
	loaded := got.HasRoot && got.BodyText != "" && len(got.Errors) == 0
	out := map[string]any{
		"ok":             loaded,
		"url":            got.URL,
		"title":          got.Title,
		"readyState":     got.Ready,
		"hasRoot":        got.HasRoot,
		"rootChildren":   got.RootKids,
		"buttons":        got.Buttons,
		"stylesheets":    got.StyleShee,
		"bodyText":       got.BodyText,
		"presetOptions":  got.PresetOptions,
		"nav":            got.Nav,
		"menu":           got.Menu,
		"diag":           got.Diag,
		"early":          a.early,
		"theme":          got.Theme,
		"pageErrors":     got.Errors,
		"resourceErrors": got.ResourceErrs,
		"hostLogTail":    strings.TrimSpace(a.log.String()),
		"profile":        a.cfg.profile,
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
// hostArgs builds the command line for the Harness host.
//
// The profile name is also the app name: `dsh web` boots the "web" profile,
// and --profile selects one explicitly. Passing both would make the second
// name a stray argument, so the profile goes in as the flag only.
func hostArgs(prefix []string, profile string, port int) []string {
	args := append([]string{}, prefix...)
	args = append(args, "--profile", profile, "--port", strconv.Itoa(port), "--no-open")
	return args
}

func (a *app) runHost(bin string, prefix []string, port int) (bool, error) {
	cmd := exec.Command(bin, hostArgs(prefix, a.cfg.profile, port)...)
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
		a.w.SetHtml(errorPage(a.cfg, title, detail, tail))
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
// the desktop's colour scheme
// ---------------------------------------------------------------------------

// systemScheme reports the desktop's colour scheme: "light", "dark", or ""
// when the desktop does not say.
//
// The XDG desktop portal is asked first: it is the cross-desktop answer --
// Plasma and GNOME both publish it -- and it is the one that survives a
// desktop whose own settings file disagrees with the GTK bridge. The GNOME
// interface key is the fallback, and it is also what the portal commonly
// proxies, so the two usually agree.
func systemScheme() string {
	if scheme := portalScheme(); scheme != "" {
		return scheme
	}
	out, err := exec.Command("gsettings", "get", "org.gnome.desktop.interface", "color-scheme").Output()
	if err != nil {
		return ""
	}
	switch strings.TrimSpace(string(out)) {
	case "'prefer-dark'":
		return "dark"
	case "'prefer-light'":
		return "light"
	}
	return ""
}

// portalScheme asks the portal for org.freedesktop.appearance color-scheme.
// The spec numbers it: 0 no preference, 1 prefer dark, 2 prefer light.
func portalScheme() string {
	out, err := exec.Command("gdbus", "call", "--session",
		"--dest", "org.freedesktop.portal.Desktop",
		"--object-path", "/org/freedesktop/portal/desktop",
		"--method", "org.freedesktop.portal.Settings.Read",
		"org.freedesktop.appearance", "color-scheme").Output()
	if err != nil {
		return ""
	}
	return portalSchemeValue(string(out))
}

// portalSchemeValue pulls the uint32 out of the portal's reply, which looks
// like "(<<uint32 2>>,)".
func portalSchemeValue(out string) string {
	m := portalUint32.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	switch m[1] {
	case "1":
		return "dark"
	case "2":
		return "light"
	}
	return ""
}

// watchSystemTheme follows the desktop's colour scheme through an event hook.
//
// WebKitGTK resolves prefers-color-scheme once, when the view is created. A
// scheme change made while the app is open never reaches the page: the media
// query does not change, so the client -- which follows the system by default
// -- keeps painting the theme it started with.
//
// Polling would fix that at the cost of waking up forever to discover nothing
// changed. So the shell subscribes instead: the portal emits SettingChanged
// the moment the desktop's scheme does, and this blocks on that stream. Two
// subscribers exist because they cover different desktops, and both are
// event-driven, so an idle window costs nothing.
func (a *app) watchSystemTheme() {
	for a.alive.Load() {
		lines, stop := subscribeScheme()
		if lines == nil {
			return // nothing to subscribe to; the boot value stands
		}
		a.themeStop.Store(stop)
		for line := range lines {
			scheme := schemeFromSignal(line)
			if scheme == "" {
				continue
			}
			a.applyScheme(scheme)
		}
		stop()
		if !a.alive.Load() {
			return
		}
		// The stream ended without the window closing, so the helper was
		// killed out from under us. Start it again after a beat.
		time.Sleep(time.Second)
	}
}

// subscribeScheme starts the cheapest available listener and returns the
// lines it emits, plus a function that stops it. A nil channel means the
// desktop offers neither.
func subscribeScheme() (<-chan string, func()) {
	type listener struct {
		bin  string
		args []string
		// match reports whether the line is a colour-scheme change at all.
		match func(string) bool
	}
	candidates := []listener{
		{
			// The portal's own stream: one line per signal, naming the
			// namespace, the key and the value.
			bin:  "gdbus",
			args: []string{"monitor", "--session", "--dest", "org.freedesktop.portal.Desktop"},
			match: func(line string) bool {
				return strings.Contains(line, "org.freedesktop.appearance")
			},
		},
		{
			bin:  "gsettings",
			args: []string{"monitor", "org.gnome.desktop.interface", "color-scheme"},
			match: func(line string) bool {
				return strings.HasPrefix(strings.TrimSpace(line), "color-scheme:")
			},
		},
	}

	for _, l := range candidates {
		if _, err := exec.LookPath(l.bin); err != nil {
			continue
		}
		cmd := exec.Command(l.bin, l.args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			continue
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true,
			// If the shell is killed outright -- SIGKILL, a crash, the
			// session manager tearing the process down -- the kernel takes
			// the listener with it instead of leaving it behind forever.
			Pdeathsig: syscall.SIGTERM,
		}
		if err := cmd.Start(); err != nil {
			continue
		}

		out := make(chan string, 4)
		go func() {
			defer close(out)
			sc := bufio.NewScanner(stdout)
			sc.Buffer(make([]byte, 0, 8<<10), 256<<10)
			for sc.Scan() {
				if l.match(sc.Text()) {
					select {
					case out <- sc.Text():
					default: // a burst of signals: the newest reading wins anyway
					}
				}
			}
		}()

		stop := func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_, _ = cmd.Process.Wait()
		}
		return out, stop
	}
	return nil, func() {}
}

// schemeFromSignal decodes one line of either stream. Both name the scheme;
// the portal numbers it and gsettings quotes it.
func schemeFromSignal(line string) string {
	if strings.Contains(line, "org.freedesktop.appearance") {
		return portalSchemeValue(line)
	}
	switch {
	case strings.Contains(line, "'prefer-dark'"):
		return "dark"
	case strings.Contains(line, "'prefer-light'"):
		return "light"
	}
	// gsettings also reports "default", which is "no preference". The portal
	// is the better authority for that case, so ask it once.
	if strings.Contains(line, "'default'") {
		return portalScheme()
	}
	return ""
}

// applyScheme tells the page which scheme the desktop is in. The page decides
// whether to act on it: a theme pinned in Settings -> Appearance is the
// user's, not the desktop's.
func (a *app) applyScheme(scheme string) {
	if !a.alive.Load() || scheme == "" {
		return
	}
	a.w.Dispatch(func() {
		a.w.Eval(`(function(){
  window.__dshDesktopScheme = ` + strconv.Quote(scheme) + `;
  if (window.__dshApplyScheme) window.__dshApplyScheme();
})();`)
	})
}

// ---------------------------------------------------------------------------
// pages
// ---------------------------------------------------------------------------

// schemeFollowJS returns the script that keeps the client on the desktop's
// colour scheme.
//
// The engine only resolves prefers-color-scheme once, so the shell watches the
// desktop instead and hands the answer in through window.__dshDesktopScheme.
// This script applies that answer, and then defends it: the client's theme
// presenter repaints from the stale media query whenever it publishes a
// snapshot, which would otherwise flip the window back to the theme it
// started in. A MutationObserver catches that and re-applies the desktop's
// answer, so the correction costs nothing while nothing is happening.
//
// It only speaks for a client that is following the system: once a theme is
// pinned in Settings -> Appearance, data-ds-theme-source stops being "system"
// and this script stands down.
func schemeFollowJS() string {
	return `  (function(){
    function apply() {
      var want = window.__dshDesktopScheme;
      if (!want) return;
      var root = document.documentElement, body = document.body;
      if (!root || !body) return;
      if (root.getAttribute('data-ds-theme-source') !== 'system') return;
      var dark = want === 'dark';
      if (body.hasAttribute('data-ds-dark-theme') === dark) return;
      body.toggleAttribute('data-ds-dark-theme', dark);
      root.style.colorScheme = want;
    }
    window.__dshApplyScheme = apply;

    function watch() {
      apply();
      if (!window.MutationObserver || !document.body) return;
      new MutationObserver(apply).observe(document.body, {
        attributes: true,
        attributeFilter: ['data-ds-dark-theme', 'data-ds-theme-source']
      });
    }
    if (document.body) watch();
    else document.addEventListener('DOMContentLoaded', watch);
  })();`
}

// schemeAttr renders the html attribute that pins the boot screen to the
// desktop's colour scheme. The media query in the stylesheet would do the same
// thing if the engine knew the answer, but the engine only looks once, at
// startup, and the desktop can change its mind after that.
func schemeAttr() string {
	if scheme := systemScheme(); scheme != "" {
		return ` data-ds-scheme="` + scheme + `"`
	}
	return ""
}

// pageStyle dresses the boot and error screens in the client's own palette so the
// window does not flash a different colour before the app paints. These are the
// values the running client actually paints, measured from the live document
// rather than guessed:
//
//	                   light      dark
//	base background    #ffffff    #151517
//	raised surface     #f5f6f7    #1b1b1c
//	border             #0000001f  #2c2c2e
//	primary label      #0f1115    #f9fafb
//	secondary label    #61666b    #cfd3d6
//	tertiary label     #81858c    #adb2b8
//
// The client follows the desktop's colour scheme by default -- its theme
// preference is "system" unless the user changes it -- so the boot screen
// follows it too. A hardcoded dark splash under a light desktop is a visible
// flash of the wrong theme on every launch, and it is the first thing the
// user sees.
//
// The accent stays DeepSeek blue, in the client's own two shades.
const pageStyle = `
  :root {
    color-scheme: light dark;
    --bg: light-dark(#ffffff, #151517);
    --surface: light-dark(#f5f6f7, #1b1b1c);
    --border: light-dark(#0000001f, #2c2c2e);
    --text: light-dark(#0f1115, #f9fafb);
    --text-2: light-dark(#61666b, #cfd3d6);
    --text-3: light-dark(#81858c, #adb2b8);
    --accent: light-dark(#4176e6, #4d93f8);
    --error: light-dark(#ec1313, #f25a5a);
  }
  /* The desktop's scheme, when the shell knows it. */
  :root[data-ds-scheme="light"] { color-scheme: light; }
  :root[data-ds-scheme="dark"] { color-scheme: dark; }
  * { box-sizing: border-box; }
  html, body { height: 100%%; margin: 0; background: var(--bg); }
  body {
    display: flex; align-items: center; justify-content: center;
    background: var(--bg); color: var(--text);
    font: 14px/1.55 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    -webkit-user-select: none; user-select: none;
  }
  .card { width: min(560px, 84vw); text-align: center; }
  h1 { font-size: 17px; font-weight: 600; margin: 0 0 6px; letter-spacing: .01em; }
  p { margin: 0; color: var(--text-2); }
  strong { color: var(--text); font-weight: 600; }
  .spinner {
    width: 26px; height: 26px; margin: 0 auto 20px;
    border: 2px solid var(--border); border-top-color: var(--accent); border-radius: 50%%;
    animation: spin .8s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  .err { border-color: var(--border); border-top-color: var(--error); animation: none; }
  pre {
    margin: 20px 0 0; padding: 14px; max-height: 34vh; overflow: auto; text-align: left;
    background: var(--surface); border: 1px solid var(--border); border-radius: 8px;
    font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
    color: var(--text-2); white-space: pre-wrap; word-break: break-word;
    -webkit-user-select: text; user-select: text;
  }
  .hint { margin-top: 18px; font-size: 12.5px; color: var(--text-3); }
  code { background: var(--surface); border: 1px solid var(--border); border-radius: 4px; padding: 1px 5px; font-size: 12px; color: var(--text-2); }
`

// pageHead carries the colour-scheme hint that stops WebKit painting a canvas
// in the wrong colour in the moment before the stylesheet above applies. It
// names both schemes, so the canvas follows the desktop rather than being
// pinned to one.
const pageHead = `<meta charset="utf-8"><meta name="color-scheme" content="light dark">`

// readableSurfacesCSS makes the client's translucent menus opaque, and follows
// the client's own light/dark theme while doing it.
//
// The client styles menu surfaces as `--dsw-menu-surface-fill` -- 45% opaque
// in the dark theme (#43454a73), 58% in the light one (#f8f9fa94) -- and leans
// on `backdrop-filter: blur(40px)` for contrast. That works on Chromium, where
// the blur composites the page behind the menu. On the WebKitGTK path this app
// uses, the blur does not composite, so the menu draws as a see-through panel
// and its labels are unreadable against the content underneath.
//
// So the blur goes, and the translucent fill is replaced by the same colour
// made opaque:
//
//	light theme   #f8f9fa94 over #fff     ->  #f8f9fa
//	dark theme    #43454a73 over #151517   ->  #2a2b2e
//
// The replacement has to follow the theme, because the labels inside the menu
// are themed too: a dark panel under light-theme labels is the very
// unreadable combination this is meant to remove, and that is what a fixed
// dark fill produces on a light desktop.
//
// The client marks the dark theme with `data-ds-dark-theme` on <body> and
// removes the attribute for the light one, so the two rules below track the
// live theme instead of guessing it at boot.
const readableSurfacesCSS = `
:root, body {
  --dsw-menu-backdrop-filter: none !important;
}
body:not([data-ds-dark-theme]) {
  --dsw-menu-surface-fill: #f8f9fa !important;
}
body[data-ds-dark-theme] {
  --dsw-menu-surface-fill: #2a2b2e !important;
}
`

// shellCSSJS returns the script that installs the shell's stylesheet into the
// document. It runs from Init, so it is in place before the client's own
// stylesheet and survives the navigation from the boot screen to the app.
func shellCSSJS(cfg config) string {
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
<html` + schemeAttr() + `><head>` + pageHead + `<title>` + appName + `</title>
<style>` + fmt.Sprintf(pageStyle) + `</style></head>
<body><div class="card">
  <div class="spinner"></div>
  <h1>Starting ` + appName + `</h1>
  <p>Booting profile <strong>` + html.EscapeString(cfg.profile) + `</strong>…</p>
  <div class="hint">First boot composes the plugin graph, so it can take a few seconds.</div>
</div></body></html>`
}

func errorPage(cfg config, title, detail, output string) string {
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
<html` + schemeAttr() + `><head>` + pageHead + `<title>` + appName + ` — error</title>
<style>` + fmt.Sprintf(pageStyle) + `</style></head>
<body>` + body + `</body></html>`
}
