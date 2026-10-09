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

// dshBaseBackground is the colour the running client paints as its base
// background, measured from the live document with -selftest. The boot and error
// screens must use it too, or the window flashes a different colour before the
// app takes over.
const dshBaseBackground = "#151517"

func TestBootScreensUseTheClientBackground(t *testing.T) {
	pages := map[string]string{
		"loading": loadingPage(config{profile: "web"}),
		"error":   errorPage("title", "detail", "some output"),
	}
	for name, page := range pages {
		if !strings.Contains(page, `name="color-scheme" content="dark"`) {
			t.Errorf("%s page lacks the colour-scheme hint that prevents a white flash", name)
		}

		// The body rule specifically must carry the client's background: a stray
		// declaration elsewhere in the stylesheet would otherwise let a wrong
		// body colour pass a naive "contains" check.
		body := ruleBody(t, page, "body")
		if !strings.Contains(body, "background: "+dshBaseBackground) {
			t.Errorf("%s page body rule does not set background: %s\n  got: %s", name, dshBaseBackground, body)
		}
		if !strings.Contains(body, "color: #f9fafb") {
			t.Errorf("%s page body rule does not use the client's primary label colour", name)
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
	page := errorPage("t", "d", `<script>alert(1)</script>`)
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
	// The upstream fill is 45% opaque and leans on a backdrop blur that does not
	// composite on this render path, which makes the menu labels unreadable. The
	// replacement must be a fully opaque colour with no alpha channel.
	if !strings.Contains(readableSurfacesCSS, "--dsw-menu-surface-fill: #2a2b2e !important") {
		t.Error("readability stylesheet does not set an opaque menu fill")
	}
	if strings.Contains(readableSurfacesCSS, "#43454a73") {
		t.Error("readability stylesheet still uses the translucent upstream fill")
	}
	if !strings.Contains(readableSurfacesCSS, "--dsw-menu-backdrop-filter: none !important") {
		t.Error("readability stylesheet does not switch off the ineffective backdrop blur")
	}

	// #2a2b2e is #43454a at 45% composited over the #151517 base background.
	// Recomputing it here documents the derivation and catches a typo.
	got := compositeOver([3]int{0x43, 0x45, 0x4a}, 0.45, [3]int{0x15, 0x15, 0x17})
	want := [3]int{0x2a, 0x2b, 0x2e}
	if got != want {
		t.Errorf("opaque equivalent computed as %v, want %v", got, want)
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

func TestReadableSurfacesIsInjectedByDefault(t *testing.T) {
	if got := readableSurfacesJS(config{opaqueMenus: true}); !strings.Contains(got, "dsh-desktop-readability") {
		t.Errorf("default config did not inject the readability stylesheet, got %q", got)
	}
	if got := readableSurfacesJS(config{opaqueMenus: false}); got != "" {
		t.Errorf("-opaque-menus=false still injected the stylesheet, got %q", got)
	}
}
