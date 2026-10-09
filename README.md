# dsh-desktop

A small native Linux desktop window for [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness).

It boots a Harness profile (`dsh web`) as a child process, reads the authenticated
loopback URL the host prints on stdout, and loads that URL in a WebKitGTK window.

```
┌─ dsh-desktop (Go, ~2.8 MB) ──────────────────────────────┐
│  spawns:  dsh web --port 0 --no-open                     │
│  reads:   "dsh web: http://127.0.0.1:37907/?token=..."   │
│  shows:   that URL in a native WebKitGTK window          │
│  quits:   kills the whole host process group             │
└──────────────────────────────────────────────────────────┘
```

## Why not Electron

| | dsh-desktop | Electron shell |
|---|---|---|
| Runtime you install | WebKitGTK + GTK 3 (already on any GTK desktop) | bundled Chromium (~150 MB) + Node |
| Binary size | 2.8 MB | 120–330 MB |
| Extra Node runtime | none — reuses your installed `dsh` | bundles a second Node |
| Memory | one WebKit render process | Chromium multi-process |
| Source | ~600 lines of Go | thousands, plus a build toolchain |

Because it does not fork or patch upstream, it shares `~/.dsh` with your CLI:
the same sessions, settings, and **agent presets**.

## Requirements

- Linux x86_64 (any distro with GTK 3 and WebKitGTK 4.1)
- Go 1.21+ to build
- `dsh` on your `PATH` (or pass `--dsh`)

Runtime packages, Debian/Ubuntu:

```sh
sudo apt install libgtk-3-0 libwebkit2gtk-4.1-0
```

Build packages:

```sh
sudo apt install golang libgtk-3-dev libwebkit2gtk-4.1-dev pkg-config
```

Fedora: `sudo dnf install golang gtk3-devel webkit2gtk4.1-devel`
Arch: `sudo pacman -S go gtk3 webkit2gtk-4.1`

## Install

```sh
./scripts/install.sh
```

Installs per user, no sudo:

| Path | What |
|---|---|
| `~/.local/bin/dsh-desktop` | the binary |
| `~/.local/share/applications/dsh-desktop.desktop` | application-menu entry |
| `~/.local/share/icons/hicolor/*/apps/dsh-desktop.png` | icons, 16→512 px |

Then launch **DeepSeek Harness** from your application menu, or run `dsh-desktop`.

Uninstall with `./scripts/install.sh --uninstall`. Your `~/.dsh` data is never touched.

## Usage

```
dsh-desktop [options]

  -profile string   Harness profile to boot (default "web")
  -port int         loopback port for the host (default 21777)
  -width, -height   initial window size (default 1280x840)
  -dsh string       path to the dsh executable
  -no-dmabuf        disable WebKit's DMA-BUF renderer (default true)
  -safe-render      additionally disable WebKit compositing mode
  -opaque-menus     make the client's translucent menus opaque (default true)
  -debug            enable the WebKit inspector
  -verbose          echo the host's output to stderr
  -selftest 25s     load, inspect the rendered page, print JSON, exit
  -version          print the version
```

### The port

The host listens on **21777** by default — above 20000, below the Linux ephemeral
range (32768–60999), so the OS is unlikely to hand the same port to something
else while the window is open. A stable port means the URL survives restarts.

`dsh` itself does **not** fall back when its port is taken; it aborts with
`EADDRINUSE`. So the app picks the port before spawning:

- **21777 free** → uses 21777.
- **21777 taken** → walks upward and uses the next free port (21778, 21779, …),
  up to 64 candidates. Every candidate stays above 20000.
- **`-port 0`** → hands the decision to the OS, which may pick any port.

A probe cannot reserve the port for the child that will actually bind it, so a
lost race is retried from the next free port rather than shown as an error.

### The `-profile` flag is the important one

Your agent presets — including **montir** — live in a profile, not in the app.
The default `web` profile is the one this machine already uses, so the desktop
window sees exactly the same preset roster and the same default as the CLI.

If you keep presets in another profile, pass it:

```sh
dsh-desktop -profile work
```

### Unreadable menus

The client styles menu surfaces as `--dsw-menu-surface-fill: #43454a73` — 45%
opaque — and leans on `backdrop-filter: blur(40px)` for contrast. That works on
Chromium, where the blur composites the page behind the menu. On this render path
the blur does not composite, so the menu draws as a see-through panel and its
labels are hard to read against the content underneath.

`-opaque-menus` (on by default) replaces the fill with the same colour made
opaque, which is what the menu looks like once the blur has done its work:

```
#43454a at 45% over the #151517 base  ->  #2a2b2e
```

The blur is switched off with it, since nothing depends on it any more. Pass
`-opaque-menus=false` to keep the upstream translucency.

### Blank window?

Some drivers (notably NVIDIA under Wayland) cannot give WebKit a DMA-BUF buffer
and the engine aborts with `Could not create GBM EGL display: EGL_SUCCESS`.
`-no-dmabuf` is therefore **on by default**. If the window still renders blank or
garbled, add `-safe-render`. To opt out entirely:

```sh
dsh-desktop -no-dmabuf=false
```

Both flags only set environment variables, and never override a value you set
yourself — so `WEBKIT_DISABLE_DMABUF_RENDERER=0 dsh-desktop` wins over the default.

## Develop

```sh
make build     # -> dist/dsh-desktop
make run       # build and launch
make test      # unit tests + full smoke test
make unit      # unit tests only (no display, no dsh needed)
make vet       # gofmt + go vet
make icons     # re-render assets/icons from assets/icon.svg
make release   # package dist/dsh-desktop-<version>-linux-x86_64.tar.gz
```

### Tests

`make unit` covers port selection, the URL pattern, and the default port range.
It needs no display and no `dsh` install, so it runs anywhere:

```
ok  github.com/folkwayse/dsh-desktop-linux
```

`make test` adds the end-to-end check. It boots an actual host, loads the actual
UI in an actual WebKit window, then reaches into the rendered DOM and reports
what it finds:

```
1. boot, load, inspect
  PASS process exited 0
  PASS window loaded a page
  PASS app root element rendered
  PASS client UI is interactive
  PASS stylesheet bundle applied
  PASS no page-level errors
  PASS montir preset on the roster
2. clean shutdown
  PASS no orphan processes left behind
```

This matters because "the process started and a window exists" is compatible
with a completely blank page. Only inspecting the live document distinguishes
the two. The orphan check compares PID sets before and after, not command-line
patterns — `pgrep -f` matches the test script's own arguments and reports
phantom orphans.

## Layout

| Path | Purpose |
|---|---|
| `main.go` | everything: flags, host supervision, WebKit window, self-test |
| `main_test.go` | unit tests for port selection and the URL pattern |
| `pkgconfig/webkit2gtk-4.0.pc` | forwards the legacy 4.0 pkg-config name to 4.1 |
| `scripts/install.sh` | build + per-user install (and `--uninstall`) |
| `scripts/smoke.sh` | the end-to-end test |
| `scripts/render-icons.py` | rasterizes `assets/icon.svg` via librsvg |
| `assets/icon.svg` | icon source — one spine (the harness), three bound plugins |

### Why the pkg-config shim

The C library vendored inside `webview_go` still asks pkg-config for
`webkit2gtk-4.0`. Ubuntu 26.04 ships only `webkit2gtk-4.1`, which is
API-compatible for everything `webview.h` touches. Rather than patch the vendored
dependency, `pkgconfig/webkit2gtk-4.0.pc` forwards the old name to the new
library. `make` and `install.sh` put that directory on `PKG_CONFIG_PATH`.

## Known limits

- **Linux only.** The Go code is portable but the window layer (WebKitGTK) is not
  wired for macOS/Windows here; those need the `webview_go` Cocoa/WebView2 paths.
- **No tray icon.** Closing the window quits the app and stops the host. The
  Electron shells keep a tray instead.
- **No auto-update.** Re-run `scripts/install.sh` after a rebuild.
- **`dsh` must resolve.** If the CLI is not on `PATH`, pass `--dsh <path>` or set
  `DSH_BIN`.
- **Single instance per profile.** A second launch refuses to start while one
  window is open for the same profile.

## Licence

MIT — see [LICENSE](LICENSE). Third-party notices, including the MIT text for the
bundled `webview_go` and the C `webview` implementation it vendors, are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
