# fyne.io/systray, patched

A copy of fyne.io/systray v1.12.2 (Apache 2.0, see LICENSE) used through a
`replace` directive in the root go.mod. The example program and tests are
dropped. One change:

- `systray_unix.go`, `argbForImage`: the StatusNotifierItem pixmap was built
  from `img.At(x, y).RGBA()` by keeping the low byte of each 16-bit,
  premultiplied channel, which scrambles every partly transparent pixel, so
  anti-aliased edges came out as coloured noise on Linux panels. It now sends
  straight 8-bit ARGB via `color.NRGBAModel`, which is what the protocol
  expects.

Drop this directory and the `replace` once upstream carries the fix.
