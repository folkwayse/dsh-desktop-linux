package main

import webview "github.com/webview/webview_go"

// Window geometry for the first paint.
//
// A fixed 1280x840 is either cramped or overwhelming depending on the screen:
// on a 1536x864 laptop it is nearly the whole display, on a 4K panel it is a
// postage stamp, and on either it lands wherever the window manager feels like
// putting it. So the window opens at a fraction of the monitor instead, inside
// the work area -- the part of the screen the desktop is not using for panels
// -- and centred there rather than under the top bar.

const (
	// The share of the screen a new window takes. Two-thirds of the area: wide
	// enough to work in, small enough to show that more is behind it.
	startWidthRatio  = 0.78
	startHeightRatio = 0.88

	// Nothing smaller is useful for a chat window with a sidebar.
	minWindowWidth  = 720
	minWindowHeight = 520
)

// rect is a position and a size. The units are whatever the caller asks in;
// for GTK they are application pixels, so a HiDPI screen needs no arithmetic.
type rect struct {
	X, Y, W, H int
}

// startSize is the size a new window should take on a screen of screenW x
// screenH with a work area of workW x workH.
//
// The order of the clamps is deliberate: the minimum is applied first and the
// work area last, so a screen too small for the minimum still produces a
// window that fits. The other order would let the work-area cap win on a tiny
// screen and then get pushed back over it by the minimum.
func startSize(screenW, screenH, workW, workH int) (width, height int) {
	width = int(startWidthRatio*float64(screenW) + 0.5)
	height = int(startHeightRatio*float64(screenH) + 0.5)

	width = max(width, minWindowWidth)
	height = max(height, minWindowHeight)

	if workW > 0 {
		width = min(width, workW)
	}
	if workH > 0 {
		height = min(height, workH)
	}
	return width, height
}

// centerIn is the top-left corner that centres a width x height window inside
// the work area.
func centerIn(work rect, width, height int) (x, y int) {
	return work.X + (work.W-width)/2, work.Y + (work.H-height)/2
}

// applyStartGeometry sizes and places the window from the primary monitor.
//
// It does nothing when there is no display to ask -- a headless run, or an
// engine built without GTK -- so the flag defaults stay in force and the app
// still starts.
func (a *app) applyStartGeometry(width, height int) {
	info, ok := primaryScreenInfo(a.w.Window())
	if !ok {
		return
	}
	a.w.SetSize(width, height, webview.HintNone)
	x, y := centerIn(info.work, width, height)
	moveWindow(a.w.Window(), x, y)
	setMinSize(a.w.Window(), min(minWindowWidth, width), min(minWindowHeight, height))
}

// windowSize is the size the window should open at: the flags when they were
// given, a share of the monitor otherwise.
func (a *app) windowSize() (width, height int) {
	if a.cfg.sizeSet {
		return a.cfg.width, a.cfg.height
	}
	if info, ok := primaryScreenInfo(a.w.Window()); ok {
		return startSize(info.screen.W, info.screen.H, info.work.W, info.work.H)
	}
	return a.cfg.width, a.cfg.height
}
