package main

import "testing"

func TestStartSizeIsAFractionOfTheScreen(t *testing.T) {
	// 78% x 88% of the monitor, rounded to the nearest pixel.
	cases := []struct {
		name             string
		screenW, screenH int
		wantW, wantH     int
	}{
		{"1920x1080", 1920, 1080, 1498, 950},
		{"2560x1440", 2560, 1440, 1997, 1267},
		{"3840x2160", 3840, 2160, 2995, 1901},
	}
	for _, c := range cases {
		w, h := startSize(c.screenW, c.screenH, c.screenW, c.screenH)
		if w != c.wantW || h != c.wantH {
			t.Errorf("%s: startSize = %dx%d, want %dx%d", c.name, w, h, c.wantW, c.wantH)
		}
	}
}

func TestStartSizeRespectsTheMinimum(t *testing.T) {
	// 78% of 800 is 624 and 88% of 600 is 528, so the width comes out at the
	// minimum and the height does not.
	w, h := startSize(800, 600, 800, 600)
	if w != minWindowWidth {
		t.Errorf("width = %d, want the minimum %d", w, minWindowWidth)
	}
	if w < minWindowWidth || h < minWindowHeight {
		t.Errorf("startSize = %dx%d, below the %dx%d minimum", w, h, minWindowWidth, minWindowHeight)
	}
}

func TestStartSizeIsCappedByTheWorkArea(t *testing.T) {
	// A taskbar eats the bottom of the screen: the window has to fit in what
	// is left rather than run under it.
	w, h := startSize(2560, 1440, 2560, 900)
	if h != 900 {
		t.Errorf("height = %d, want the work area height 900", h)
	}
	if w != 1997 {
		t.Errorf("width = %d, want 1997 (the work area is wide enough)", w)
	}
}

func TestStartSizeFitsATinyWorkArea(t *testing.T) {
	// The minimum is applied before the work-area cap, so a work area smaller
	// than the minimum still wins instead of panicking on an inverted clamp.
	w, h := startSize(640, 480, 640, 400)
	if w != 640 || h != 400 {
		t.Errorf("startSize = %dx%d, want 640x400 (the work area)", w, h)
	}
	if w > 640 || h > 400 {
		t.Errorf("startSize = %dx%d overflows the work area", w, h)
	}
}

func TestStartSizeIgnoresAnUnknownWorkArea(t *testing.T) {
	// A display that cannot report its work area reports 0; the screen size
	// then decides, not a zero-sized cap.
	w, h := startSize(1920, 1080, 0, 0)
	if w != 1498 || h != 950 {
		t.Errorf("startSize = %dx%d, want 1498x950", w, h)
	}
}

func TestCenterIn(t *testing.T) {
	cases := []struct {
		name  string
		work  rect
		w, h  int
		wantX int
		wantY int
	}{
		{"plain", rect{0, 0, 1920, 1080}, 1000, 600, 460, 240},
		{"offset work area", rect{100, 50, 1920, 1000}, 1000, 600, 560, 250},
		{"exact fit", rect{0, 0, 800, 600}, 800, 600, 0, 0},
	}
	for _, c := range cases {
		x, y := centerIn(c.work, c.w, c.h)
		if x != c.wantX || y != c.wantY {
			t.Errorf("%s: centerIn = %d,%d, want %d,%d", c.name, x, y, c.wantX, c.wantY)
		}
	}
}

func TestExplicitSizeWinsOverTheMonitor(t *testing.T) {
	// -width/-height are an instruction, not a suggestion: once either is
	// given the window keeps that size whatever the screen is.
	cfg, err := parseFlags([]string{"-width", "1000"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !cfg.sizeSet {
		t.Error("-width did not mark the size as explicit")
	}
	if cfg.width != 1000 {
		t.Errorf("width = %d, want 1000", cfg.width)
	}

	// The same for the height alone.
	cfg, err = parseFlags([]string{"-height", "600"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !cfg.sizeSet {
		t.Error("-height did not mark the size as explicit")
	}

	// Neither flag: the monitor decides.
	cfg, err = parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.sizeSet {
		t.Error("no size flags, but the size was marked explicit")
	}
}
