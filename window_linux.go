//go:build linux

package main

// Querying the desktop and placing the window is the one thing the webview
// wrapper does not expose: it can size a window but not ask where the screen
// is, and it has no notion of a work area. GTK is already linked -- the webview
// is a WebKitGTK widget -- so the few calls needed live here rather than in a
// second toolkit.

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>
#include <gdk/gdk.h>

// dsh_primary_monitor returns the primary monitor, or the first one on a
// display that does not mark a primary (X11 without a RandR primary).
static GdkMonitor *dsh_primary_monitor(GtkWidget *win) {
  GdkDisplay *display = gtk_widget_get_display(win);
  if (!display) return NULL;
  GdkMonitor *monitor = gdk_display_get_primary_monitor(display);
  if (!monitor && gdk_display_get_n_monitors(display) > 0) {
    monitor = gdk_display_get_monitor(display, 0);
  }
  return monitor;
}

// dsh_screen_info fills the monitor geometry and its work area. Both are in
// application pixels, which is the same unit gtk_window_resize and
// gtk_window_move take, so no scale factor arithmetic is needed here.
static int dsh_screen_info(GtkWidget *win, int *sx, int *sy, int *sw, int *sh,
                           int *wx, int *wy, int *ww, int *wh) {
  GdkMonitor *monitor = dsh_primary_monitor(win);
  if (!monitor) return 0;

  GdkRectangle geometry = {0, 0, 0, 0};
  GdkRectangle workarea = {0, 0, 0, 0};
  gdk_monitor_get_geometry(monitor, &geometry);
  gdk_monitor_get_workarea(monitor, &workarea);

  // Compositors that do not publish a work area (notably Wayland) report an
  // empty rectangle. The screen itself is then the best available answer.
  if (workarea.width <= 0 || workarea.height <= 0) workarea = geometry;
  if (geometry.width <= 0 || geometry.height <= 0) return 0;

  *sx = geometry.x;   *sy = geometry.y;
  *sw = geometry.width; *sh = geometry.height;
  *wx = workarea.x;   *wy = workarea.y;
  *ww = workarea.width; *wh = workarea.height;
  return 1;
}

// dsh_move_window asks the window manager to put the window at a position.
// Wayland compositors ignore this by design; X11 honours it.
static void dsh_move_window(GtkWidget *win, int x, int y) {
  gtk_window_move(GTK_WINDOW(win), x, y);
}

// dsh_set_min_size keeps the window usable when the screen is small.
static void dsh_set_min_size(GtkWidget *win, int width, int height) {
  GdkGeometry hints = {0};
  hints.min_width = width;
  hints.min_height = height;
  gtk_window_set_geometry_hints(GTK_WINDOW(win), NULL, &hints, GDK_HINT_MIN_SIZE);
}
*/
import "C"

import "unsafe"

// screenInfo is one monitor and the part of it a window may occupy: the work
// area excludes whatever the desktop keeps for panels and taskbars.
type screenInfo struct {
	screen rect
	work   rect
}

// primaryScreenInfo reads the primary monitor's geometry and work area.
// It reports false when there is no display to ask.
func primaryScreenInfo(win unsafe.Pointer) (screenInfo, bool) {
	if win == nil {
		return screenInfo{}, false
	}
	var sx, sy, sw, sh, wx, wy, ww, wh C.int
	if C.dsh_screen_info((*C.GtkWidget)(win), &sx, &sy, &sw, &sh, &wx, &wy, &ww, &wh) == 0 {
		return screenInfo{}, false
	}
	return screenInfo{
		screen: rect{X: int(sx), Y: int(sy), W: int(sw), H: int(sh)},
		work:   rect{X: int(wx), Y: int(wy), W: int(ww), H: int(wh)},
	}, true
}

// moveWindow places the window. On Wayland the compositor decides and ignores
// the request; that is not an error, only a different window manager policy.
func moveWindow(win unsafe.Pointer, x, y int) {
	if win == nil {
		return
	}
	C.dsh_move_window((*C.GtkWidget)(win), C.int(x), C.int(y))
}

// setMinSize stops the window being dragged smaller than the boot screen can
// render.
func setMinSize(win unsafe.Pointer, width, height int) {
	if win == nil {
		return
	}
	C.dsh_set_min_size((*C.GtkWidget)(win), C.int(width), C.int(height))
}
