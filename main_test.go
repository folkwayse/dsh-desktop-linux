package main

import (
	"net"
	"strconv"
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
