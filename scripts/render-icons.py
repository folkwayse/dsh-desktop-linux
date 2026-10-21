#!/usr/bin/env python3
"""Rasterize the icon master into the PNG sizes a Linux icon theme needs.

The master is `assets/icon.svg` when a vector is available, and
`assets/icon.png` otherwise. A vector is rendered directly at each size, so
every one is crisp. A bitmap is halved repeatedly down to the requested size
rather than scaled in one jump, because a 512 px source sampled straight to
16 px loses the shape in a blur.

Only librsvg through the gdk-pixbuf loader is required for the vector path,
which a GTK desktop already ships.
"""

import os
import sys

import gi

gi.require_version("GdkPixbuf", "2.0")
from gi.repository import GdkPixbuf  # noqa: E402

SIZES = (16, 24, 32, 48, 64, 128, 256, 512)
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def master_path():
    """The icon source, preferring a vector when the repository has one."""
    for name in ("icon.svg", "icon.png"):
        path = os.path.join(ROOT, "assets", name)
        if os.path.exists(path):
            return path
    return None


def scale_down(pixbuf, size):
    """Halve repeatedly before the last step, which keeps edges from smearing."""
    while pixbuf.get_width() > size * 2:
        next_size = max(size, pixbuf.get_width() // 2)
        pixbuf = pixbuf.scale_simple(
            next_size, next_size, GdkPixbuf.InterpType.BILINEAR
        )
    return pixbuf.scale_simple(size, size, GdkPixbuf.InterpType.BILINEAR)


def render(source, size, out_path):
    if source.endswith(".svg"):
        pixbuf = GdkPixbuf.Pixbuf.new_from_file_at_scale(source, size, size, True)
    else:
        pixbuf = scale_down(GdkPixbuf.Pixbuf.new_from_file(source), size)
    pixbuf.savev(out_path, "png", [], [])


def main():
    source = master_path()
    if source is None:
        print("missing assets/icon.svg or assets/icon.png", file=sys.stderr)
        return 1

    out_dir = os.path.join(ROOT, "assets", "icons")
    os.makedirs(out_dir, exist_ok=True)

    print(f"master: {os.path.relpath(source, ROOT)}")
    for size in SIZES:
        path = os.path.join(out_dir, f"icon-{size}.png")
        render(source, size, path)
        print(f"{path}  {os.path.getsize(path)} bytes")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
