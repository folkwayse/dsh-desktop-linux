#!/usr/bin/env bash
# Build dsh-desktop and install it for the current user.
#
# Everything lands under $HOME, so no sudo is needed:
#   ~/.local/bin/dsh-desktop
#   ~/.local/share/applications/dsh-desktop.desktop
#   ~/.local/share/icons/hicolor/<size>/apps/dsh-desktop.png
#
# Re-run after a rebuild to upgrade in place. Pass --uninstall to remove.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${HOME}/.local/bin"
APP_DIR="${HOME}/.local/share/applications"
ICON_ROOT="${HOME}/.local/share/icons/hicolor"
DESKTOP_ID="dsh-desktop"

say() { printf '  %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

uninstall() {
  echo "Removing dsh-desktop:"
  rm -f "${BIN_DIR}/dsh-desktop" && say "removed ${BIN_DIR}/dsh-desktop"
  rm -f "${APP_DIR}/${DESKTOP_ID}.desktop" && say "removed ${APP_DIR}/${DESKTOP_ID}.desktop"
  for f in "${ICON_ROOT}"/*/apps/${DESKTOP_ID}.png; do
    [ -e "$f" ] || continue
    rm -f "$f" && say "removed $f"
  done
  command -v update-desktop-database >/dev/null && \
    update-desktop-database "$APP_DIR" >/dev/null 2>&1 || true
  command -v gtk-update-icon-cache >/dev/null && \
    gtk-update-icon-cache -qtf "$ICON_ROOT" >/dev/null 2>&1 || true
  echo "Done. Your sessions and settings in ~/.dsh were not touched."
}

if [ "${1:-}" = "--uninstall" ]; then
  uninstall
  exit 0
fi

# ---------------------------------------------------------------------------
# locate or build the binary
# ---------------------------------------------------------------------------
#
# This script ships in two places, so it must work in both:
#   - a source checkout, where it builds from main.go
#   - the release tarball, which has no source and only a prebuilt binary
cd "$ROOT"
mkdir -p dist

if [ -f go.mod ]; then
  command -v go >/dev/null || die "Go is required to build: https://go.dev/dl/"
  command -v pkg-config >/dev/null || die "pkg-config is required"

  if ! pkg-config --exists gtk+-3.0; then
    die "GTK 3 development files are missing. On Debian/Ubuntu:
       sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev"
  fi
  if ! pkg-config --exists webkit2gtk-4.1; then
    die "WebKitGTK 4.1 development files are missing. On Debian/Ubuntu:
       sudo apt install libwebkit2gtk-4.1-dev"
  fi

  echo "Building dsh-desktop:"

  # webview's bundled C library asks pkg-config for webkit2gtk-4.0, which modern
  # distributions no longer ship. pkgconfig/webkit2gtk-4.0.pc forwards to 4.1,
  # which is API-compatible for everything webview.h uses.
  export PKG_CONFIG_PATH="${ROOT}/pkgconfig${PKG_CONFIG_PATH:+:${PKG_CONFIG_PATH}}"

  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)"
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o dist/dsh-desktop .
  say "built dist/dsh-desktop ($(du -h dist/dsh-desktop | cut -f1), version ${VERSION})"
elif [ -x "${ROOT}/dsh-desktop" ]; then
  echo "Using the prebuilt binary from this tarball."
  cp "${ROOT}/dsh-desktop" dist/dsh-desktop
else
  die "found neither go.mod (to build) nor a prebuilt ./dsh-desktop (to install)"
fi

# The binary is what actually needs the runtime libraries, so check those even
# when nothing was compiled here.
if ! command -v pkg-config >/dev/null || ! pkg-config --exists webkit2gtk-4.1; then
  say "warning: WebKitGTK 4.1 was not found. Install it before launching:"
  say "  sudo apt install libwebkit2gtk-4.1-0"
fi

# ---------------------------------------------------------------------------
# icons
# ---------------------------------------------------------------------------

if [ ! -f assets/icons/icon-256.png ]; then
  if [ -f scripts/render-icons.py ] && command -v python3 >/dev/null; then
    python3 scripts/render-icons.py >/dev/null && say "rendered icons"
  else
    say "warning: no icons available; the launcher will use a generic icon"
  fi
fi

# ---------------------------------------------------------------------------
# install
# ---------------------------------------------------------------------------

mkdir -p "$BIN_DIR" "$APP_DIR"

install -m 0755 dist/dsh-desktop "${BIN_DIR}/dsh-desktop"
say "installed ${BIN_DIR}/dsh-desktop"

for src in assets/icons/icon-*.png; do
  [ -e "$src" ] || continue
  size="$(basename "$src" .png | sed 's/^icon-//')"
  dest="${ICON_ROOT}/${size}x${size}/apps"
  mkdir -p "$dest"
  install -m 0644 "$src" "${dest}/${DESKTOP_ID}.png"
done
say "installed icons under ${ICON_ROOT}"

cat > "${APP_DIR}/${DESKTOP_ID}.desktop" <<EOF
[Desktop Entry]
Type=Application
Version=1.0
Name=DeepSeek Harness
GenericName=AI coding agent
Comment=Native desktop window for DeepSeek Harness
Exec=${BIN_DIR}/dsh-desktop %U
Icon=${DESKTOP_ID}
Terminal=false
Categories=Development;
Keywords=dsh;deepseek;harness;ai;agent;coding;
StartupNotify=true
StartupWMClass=dsh-desktop
EOF
say "installed ${APP_DIR}/${DESKTOP_ID}.desktop"

command -v update-desktop-database >/dev/null && \
  update-desktop-database "$APP_DIR" >/dev/null 2>&1 || true
command -v gtk-update-icon-cache >/dev/null && \
  gtk-update-icon-cache -qtf "$ICON_ROOT" >/dev/null 2>&1 || true

echo
echo "Installed. Launch \"DeepSeek Harness\" from your application menu, or run:"
echo "  dsh-desktop"
case ":${PATH}:" in
  *":${BIN_DIR}:"*) ;;
  *) echo
     echo "Note: ${BIN_DIR} is not on your PATH. Add it with:"
     echo "  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile" ;;
esac
