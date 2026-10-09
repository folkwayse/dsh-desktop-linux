# Third-party notices

This project is distributed as a compiled binary, so the licences of the code
linked into that binary are reproduced here.

## webview_go

Bundled at build time as a Go module (`github.com/webview/webview_go`). It wraps
the platform webview library and vendors the C/C++ `webview` implementation that
the binary links against.

```
MIT License

Copyright (c) 2017 Serge Zaitsev
Copyright (c) 2020 webview

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## System libraries

The binary links dynamically against GTK 3 and WebKitGTK 4.1, which are covered
by the GNU LGPL. They are not redistributed here; users install them through
their distribution's package manager.

## DeepSeek Harness

Not included and not modified. This program runs the `dsh` command that the user
has installed. DeepSeek Harness is MIT licensed, copyright (c) 2026 DeepSeek.
