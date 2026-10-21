package main

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// occupy binds a loopback port and keeps it open for the duration of the test,
// so pickPort sees it as taken.
func occupy(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("cannot occupy port %d to set up the test: %v", port, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func TestDefaultPortIsAboveTwentyThousand(t *testing.T) {
	// The whole point of the chosen default: above 20000, below the Linux
	// ephemeral range, so the OS is unlikely to hand it to something else.
	if defaultPort <= 20000 {
		t.Errorf("defaultPort = %d, want above 20000", defaultPort)
	}
	if defaultPort >= 32768 {
		t.Errorf("defaultPort = %d, want below the ephemeral range (32768)", defaultPort)
	}
}

func TestPickPortPassesZeroThrough(t *testing.T) {
	// Zero means "let the OS choose", so it must reach the host unchanged.
	got, err := pickPort(0)
	if err != nil {
		t.Fatalf("pickPort(0) returned an error: %v", err)
	}
	if got != 0 {
		t.Errorf("pickPort(0) = %d, want 0", got)
	}
}

func TestPickPortReturnsPreferredWhenFree(t *testing.T) {
	const want = 24517
	got, err := pickPort(want)
	if err != nil {
		t.Fatalf("pickPort(%d) returned an error: %v", want, err)
	}
	if got != want {
		t.Errorf("pickPort(%d) = %d, want the preferred port back", want, got)
	}
}

func TestPickPortSkipsOccupiedPorts(t *testing.T) {
	const base = 24600
	occupy(t, base)
	occupy(t, base+1)

	got, err := pickPort(base)
	if err != nil {
		t.Fatalf("pickPort(%d) returned an error: %v", base, err)
	}
	if got != base+2 {
		t.Errorf("pickPort(%d) = %d, want %d (the first free port)", base, got, base+2)
	}
}

func TestPickPortStaysAboveTwentyThousand(t *testing.T) {
	// Scanning upward from a >20000 base must never land in the low range.
	const base = 24700
	occupy(t, base)
	got, err := pickPort(base)
	if err != nil {
		t.Fatalf("pickPort(%d) returned an error: %v", base, err)
	}
	if got <= 20000 {
		t.Errorf("pickPort(%d) = %d, want a port above 20000", base, got)
	}
}

func TestPickPortGivesUpWhenRangeIsFull(t *testing.T) {
	const base = 24800
	for port := base; port < base+portScanLimit; port++ {
		occupy(t, port)
	}
	if _, err := pickPort(base); err == nil {
		t.Errorf("pickPort(%d) succeeded with all %d ports taken, want an error", base, portScanLimit)
	}
}

func TestPortFreeRejectsOccupiedPort(t *testing.T) {
	const port = 24900
	occupy(t, port)
	if portFree(port) {
		t.Errorf("portFree(%d) = true while the port is bound", port)
	}
}

func TestURLPatternExtractsTheTokenisedURL(t *testing.T) {
	line := "dsh web: http://127.0.0.1:21777/?token=P97Zb7vY1rzc8EhMfxl_3J0bTMlqQHB4a9FEXK2xCj8"
	got := urlPattern.FindString(line)
	want := "http://127.0.0.1:21777/?token=P97Zb7vY1rzc8EhMfxl_3J0bTMlqQHB4a9FEXK2xCj8"
	if got != want {
		t.Errorf("urlPattern found %q, want %q", got, want)
	}
}

func TestURLPatternIgnoresLinesWithoutAToken(t *testing.T) {
	// The host prints plenty of other output; only the tokenised line counts.
	for _, line := range []string{
		"dsh web: http://127.0.0.1:21777/",
		"listening on 127.0.0.1:21777",
		"",
	} {
		if got := urlPattern.FindString(line); got != "" {
			t.Errorf("urlPattern matched %q in %q, want no match", got, line)
		}
	}
}

// The colours the running client paints, measured from the live document with
// -selftest. The boot and error screens must use them too, or the window
// flashes a different colour before the app takes over. The client follows the
// desktop's scheme, so each one has a light and a dark value.
var dshPalette = map[string]struct{ light, dark string }{
	"base background": {"#ffffff", "#151517"},
	"raised surface":  {"#f5f6f7", "#1b1b1c"},
	"primary label":   {"#0f1115", "#f9fafb"},
	"secondary label": {"#61666b", "#cfd3d6"},
	"tertiary label":  {"#81858c", "#adb2b8"},
}

func TestBootScreensUseTheClientPalette(t *testing.T) {
	pages := map[string]string{
		"loading": loadingPage(config{profile: "web"}),
		"error":   errorPage(config{profile: "web"}, "title", "detail", "some output"),
	}
	for name, page := range pages {
		if !strings.Contains(page, `name="color-scheme" content="light dark"`) {
			t.Errorf("%s page lacks the colour-scheme hint that prevents a flash of the wrong theme", name)
		}

		// The body rule specifically must carry the client's background: a stray
		// declaration elsewhere in the stylesheet would otherwise let a wrong
		// body colour pass a naive "contains" check.
		body := ruleBody(t, page, "body")
		if !strings.Contains(body, "background: var(--bg)") {
			t.Errorf("%s page body rule does not use the client's background\n  got: %s", name, body)
		}
		if !strings.Contains(body, "color: var(--text)") {
			t.Errorf("%s page body rule does not use the client's primary label colour", name)
		}
	}

	// Both schemes carry the whole palette, so the boot screen matches the app
	// whichever scheme the desktop is in.
	for what, colours := range dshPalette {
		for scheme, value := range map[string]string{"light": colours.light, "dark": colours.dark} {
			if !strings.Contains(pageStyle, value) {
				t.Errorf("boot page is missing the %s %s colour %s", scheme, what, value)
			}
		}
	}
}

// ruleBody extracts the declarations of the first `selector { ... }` block that
// declares no nested block, which is enough for the flat stylesheet used here.
func ruleBody(t *testing.T, page, selector string) string {
	t.Helper()
	idx := strings.Index(page, "\n  "+selector+" {")
	if idx < 0 {
		t.Fatalf("no %q rule in the page", selector)
	}
	open := idx + strings.Index(page[idx:], "{") + 1
	close := strings.Index(page[open:], "}")
	if close < 0 {
		t.Fatalf("unterminated %q rule", selector)
	}
	return page[open : open+close]
}

// TestNoStalePaletteColours guards the actual regression: the boot screens once
// used a palette of their own, which made the window flash a different colour
// before the app painted.
func TestNoStalePaletteColours(t *testing.T) {
	stale := []string{"#0b0d10", "#e6e8eb", "#8b929c", "#14171b", "#23272d", "#6b727c", "#9aa1ab", "#24282e"}
	for _, c := range stale {
		if strings.Contains(pageStyle, c) {
			t.Errorf("boot screen stylesheet still uses the stale colour %s", c)
		}
	}
}

func TestErrorPageEscapesHostOutput(t *testing.T) {
	// Host output is untrusted text; it must not be able to inject markup.
	page := errorPage(config{}, "t", "d", `<script>alert(1)</script>`)
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("error page rendered raw host output as markup")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("error page did not escape the host output")
	}
}

func TestLoadingPageNamesTheProfile(t *testing.T) {
	page := loadingPage(config{profile: "work"})
	if !strings.Contains(page, "<strong>work</strong>") {
		t.Error("loading page does not name the profile it is booting")
	}
}

func TestReadableSurfacesMakesTheMenuOpaque(t *testing.T) {
	// The upstream fill is translucent and leans on a backdrop blur that does not
	// composite on this render path, which makes the menu labels unreadable. The
	// replacement must be a fully opaque colour with no alpha channel -- and it
	// must follow the theme, because the labels inside the menu are themed too.
	if !strings.Contains(readableSurfacesCSS, "--dsw-menu-backdrop-filter: none !important") {
		t.Error("readability stylesheet does not switch off the ineffective backdrop blur")
	}
	for _, translucent := range []string{"#43454a73", "#f8f9fa94"} {
		if strings.Contains(readableSurfacesCSS, translucent) {
			t.Errorf("readability stylesheet still uses the translucent upstream fill %s", translucent)
		}
	}
	if !strings.Contains(readableSurfacesCSS, "body[data-ds-dark-theme]") {
		t.Error("readability stylesheet does not follow the client's dark theme")
	}
	if !strings.Contains(readableSurfacesCSS, "body:not([data-ds-dark-theme])") {
		t.Error("readability stylesheet does not follow the client's light theme")
	}

	// The dark replacement, #2a2b2e, is #43454a at 45% composited over the
	// #151517 base background. Recomputing it here documents the derivation and
	// catches a typo.
	dark := compositeOver([3]int{0x43, 0x45, 0x4a}, 0.45, [3]int{0x15, 0x15, 0x17})
	if dark != [3]int{0x2a, 0x2b, 0x2e} {
		t.Errorf("opaque equivalent computed as %v, want %v", dark, [3]int{0x2a, 0x2b, 0x2e})
	}
	if !strings.Contains(readableSurfacesCSS, "#2a2b2e !important") {
		t.Error("readability stylesheet does not use the opaque dark fill")
	}

	// The light replacement, #f8f9fa, is the client's own opaque menu fill
	// (#f8f9faf0, what it uses on macOS) flattened over the page background.
	light := compositeOver([3]int{0xf8, 0xf9, 0xfa}, 0.94, [3]int{0xff, 0xff, 0xff})
	if light != [3]int{0xf8, 0xf9, 0xfa} {
		t.Errorf("opaque light equivalent computed as %v, want %v", light, [3]int{0xf8, 0xf9, 0xfa})
	}
	if !strings.Contains(readableSurfacesCSS, "#f8f9fa !important") {
		t.Error("readability stylesheet does not use the opaque light fill")
	}
}

// compositeOver blends a colour at the given alpha over an opaque background.
func compositeOver(fg [3]int, alpha float64, bg [3]int) [3]int {
	var out [3]int
	for i := range fg {
		out[i] = int(float64(fg[i])*alpha + float64(bg[i])*(1-alpha) + 0.5)
	}
	return out
}

func TestShellCSSIsInjectedByDefault(t *testing.T) {
	if got := shellCSSJS(config{opaqueMenus: true}); !strings.Contains(got, "dsh-desktop-readability") {
		t.Errorf("default config did not inject the readability stylesheet, got %q", got)
	}
	if got := shellCSSJS(config{opaqueMenus: false}); got != "" {
		t.Errorf("-opaque-menus=false still injected a stylesheet, got %q", got)
	}
}

func TestPortalSchemeValue(t *testing.T) {
	// The XDG spec numbers the preference: 0 none, 1 dark, 2 light.
	cases := map[string]string{
		"(<<uint32 1>>,)": "dark",
		"(<<uint32 2>>,)": "light",
		"(<<uint32 0>>,)": "",
		"":                "",
		"garbage":         "",
	}
	for in, want := range cases {
		if got := portalSchemeValue(in); got != want {
			t.Errorf("portalSchemeValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSchemeFromSignal(t *testing.T) {
	cases := map[string]string{
		// The portal's stream names the appearance namespace and the number.
		"/org/freedesktop/portal/desktop: org.freedesktop.portal.Settings.SettingChanged " +
			"('org.freedesktop.appearance', 'color-scheme', <uint32 1>)": "dark",
		"/org/freedesktop/portal/desktop: org.freedesktop.portal.Settings.SettingChanged " +
			"('org.freedesktop.appearance', 'color-scheme', <uint32 2>)": "light",
		// gsettings quotes the preference.
		"color-scheme: 'prefer-dark'":  "dark",
		"color-scheme: 'prefer-light'": "light",
		// Unrelated traffic is ignored.
		"('org.kde.VirtualKeyboard', 'active', <true>)": "",
	}
	for in, want := range cases {
		if got := schemeFromSignal(in); got != want {
			t.Errorf("schemeFromSignal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBootPagesFollowTheDesktopTheme(t *testing.T) {
	// The client follows the desktop's colour scheme, so the boot and error
	// screens have to as well: a hardcoded dark splash under a light desktop is
	// a flash of the wrong theme on every launch.
	if !strings.Contains(pageStyle, "light-dark(") {
		t.Error("boot page does not resolve its palette per colour scheme")
	}
	if !strings.Contains(pageStyle, "color-scheme: light dark") {
		t.Error("boot page does not follow the desktop's scheme")
	}
	// The engine only resolves prefers-color-scheme once, so the shell pins
	// the boot screen to the desktop's scheme itself.
	for _, scheme := range []string{`[data-ds-scheme="light"]`, `[data-ds-scheme="dark"]`} {
		if !strings.Contains(pageStyle, scheme) {
			t.Errorf("boot page does not honour the desktop's scheme: %s missing", scheme)
		}
	}
	for _, colour := range []string{"#ffffff", "#151517", "#0f1115", "#f9fafb"} {
		if !strings.Contains(pageStyle, colour) {
			t.Errorf("boot page is missing the palette colour %s", colour)
		}
	}
	if !strings.Contains(pageHead, "light dark") {
		t.Error("boot page head does not name both colour schemes")
	}
}

func TestHostArgsCarryTheProfile(t *testing.T) {
	// -profile has to reach the host: it selects the profile under
	// $DSH_HOME/profiles, and the profile name is also the app name.
	args := hostArgs(nil, "work", 21777)
	want := []string{"--profile", "work", "--port", "21777", "--no-open"}
	if len(args) != len(want) {
		t.Fatalf("host args are %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("host args are %v, want %v", args, want)
		}
	}
	// The node fallback prepends the CLI entry point.
	args = hostArgs([]string{"/opt/dsh/bin.js"}, "web", 0)
	want = []string{"/opt/dsh/bin.js", "--profile", "web", "--port", "0", "--no-open"}
	if len(args) != len(want) {
		t.Fatalf("host args are %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("host args are %v, want %v", args, want)
		}
	}
}
