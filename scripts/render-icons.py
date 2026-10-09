#!/usr/bin/env python3
"""Rasterize assets/icon.svg into the PNG sizes a Linux icon theme needs.

Uses librsvg through the gdk-pixbuf SVG loader, so the only requirement is the
librsvg pixbuf loader that a GTK desktop already ships. The loader renders the
vector at the requested size rather than scaling a bitmap, so every size is
crisp.
"""

import os
import sys

import gi

gi.require_version("GdkPixbuf", "2.0")
from gi.repository import GdkPixbuf  # noqa: E402

SIZES = (16, 24, 32, 48, 64, 128, 256, 512)
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def render(svg_path, size, out_path):
    pixbuf = GdkPixbuf.Pixbuf.new_from_file_at_scale(svg_path, size, size, True)
    pixbuf.savev(out_path, "png", [], [])


def main():
    svg_path = os.path.join(ROOT, "assets", "icon.svg")
    if not os.path.exists(svg_path):
        print("missing", svg_path, file=sys.stderr)
        return 1

    out_dir = os.path.join(ROOT, "assets", "icons")
    os.makedirs(out_dir, exist_ok=True)

    for size in SIZES:
        path = os.path.join(out_dir, f"icon-{size}.png")
        render(svg_path, size, path)
        print(f"{path}  {os.path.getsize(path)} bytes")

    # A copy next to the assets, handy for docs and the desktop entry preview.
    render(svg_path, 256, os.path.join(ROOT, "assets", "icon.png"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
