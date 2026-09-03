package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"runtime"
)

// iconSize is the rendered pixel size. macOS scales it to 16 points; Linux
// tray hosts scale the pixmap to the panel height.
const iconSize = 32

// stateColors are the Linux tray colours per state. macOS gets a template
// icon instead: black with alpha, which the menu bar recolours for light and
// dark mode. There the state shows in the shape, not the colour.
var stateColors = map[State]color.NRGBA{
	StateStopped:      {R: 0x8a, G: 0x8a, B: 0x8a, A: 0xff},
	StateUp:           {R: 0x2f, G: 0xa8, B: 0x4f, A: 0xff},
	StateReconnecting: {R: 0xe0, G: 0xa0, B: 0x20, A: 0xff},
	StateFailed:       {R: 0xd9, G: 0x48, B: 0x3b, A: 0xff},
}

// Icon renders the tray glyph for a state as PNG bytes. The glyph is a ring (a
// tunnel mouth seen head-on) whose centre says what is happening: empty when
// idle, a full dot when up, a small dot while connecting, a bar when failed.
func Icon(state State, template bool) []byte {
	tint := stateColors[state]
	if template {
		tint = color.NRGBA{A: 0xff}
	}

	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	const samples = 4
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			var hit int
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/samples
					py := float64(y) + (float64(sy)+0.5)/samples
					if inGlyph(state, px, py) {
						hit++
					}
				}
			}
			if hit == 0 {
				continue
			}
			c := tint
			c.A = uint8(float64(tint.A) * float64(hit) / (samples * samples))
			img.SetNRGBA(x, y, c)
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// trayIcons returns the two icons the tray wants: a template for macOS and a
// coloured one for everything else. Windows loads icons through the ICO
// loader, so the PNG is wrapped as a one-image ICO there; Vista and later read
// PNG-in-ICO directly.
func trayIcons(state State) (template, regular []byte) {
	template, regular = Icon(state, true), Icon(state, false)
	if runtime.GOOS == "windows" {
		regular = icoWrap(regular, iconSize)
	}
	return template, regular
}

// icoWrap puts one PNG into an ICO container.
func icoWrap(pngData []byte, size int) []byte {
	var buf bytes.Buffer
	// ICONDIR: reserved, type 1 (icon), one image.
	_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, 1})
	// ICONDIRENTRY: width, height, colours, reserved, planes, bit count, size, offset.
	buf.WriteByte(byte(size))
	buf.WriteByte(byte(size))
	buf.WriteByte(0)
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.LittleEndian, [2]uint16{1, 32})
	_ = binary.Write(&buf, binary.LittleEndian, [2]uint32{uint32(len(pngData)), 6 + 16})
	buf.Write(pngData)
	return buf.Bytes()
}

// inGlyph reports whether a point (in pixel space) is inside the glyph.
func inGlyph(state State, px, py float64) bool {
	const centre = iconSize / 2.0
	dx, dy := px-centre, py-centre
	d := math.Hypot(dx, dy)

	const outer, inner = 13.5, 9.5
	if d <= outer && d >= inner {
		return true
	}
	switch state {
	case StateUp:
		return d <= 5
	case StateReconnecting, StateConnecting:
		return d <= 2.75
	case StateFailed:
		return math.Abs(dy) <= 1.75 && math.Abs(dx) <= 5.5
	}
	return false
}
