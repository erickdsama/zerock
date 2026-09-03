package tray

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"runtime"
	"sync"
)

// markPNG is the zerock mark at iconSize, scaled from assets/icon.png, the
// mark as supplied. Regenerate it with:
//
//	convert icon.png -background none -gravity center -extent 180x180 \
//	  -filter Lanczos -resize 64x64 -extent 64x64 mark.png
//
//go:embed assets/mark.png
var markPNG []byte

// iconSize is the rendered pixel size. macOS scales it to 16 points, Linux
// tray hosts to the panel height, Windows to its small-icon size.
const iconSize = 64

var (
	markOnce sync.Once
	mark     *image.NRGBA
)

// loadMark decodes the embedded mark once.
func loadMark() *image.NRGBA {
	markOnce.Do(func() {
		img, err := png.Decode(bytes.NewReader(markPNG))
		if err != nil {
			panic("tray: embedded mark.png is not a PNG: " + err.Error())
		}
		mark = image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
		draw.Draw(mark, mark.Bounds(), img, img.Bounds().Min, draw.Src)
	})
	return mark
}

// badgeColors are the status badge colours per state. Stopped has no badge.
var badgeColors = map[State]color.NRGBA{
	StateUp:           {R: 0x2f, G: 0xa8, B: 0x4f, A: 0xff},
	StateConnecting:   {R: 0xe0, G: 0xa0, B: 0x20, A: 0xff},
	StateReconnecting: {R: 0xe0, G: 0xa0, B: 0x20, A: 0xff},
	StateFailed:       {R: 0xd9, G: 0x48, B: 0x3b, A: 0xff},
}

// Badge geometry, in pixels at iconSize: bottom-right corner, with a
// transparent gap around it so it reads against the mark.
const (
	badgeCX, badgeCY = 51.5, 51.5
	badgeR           = 10.5
	badgeGap         = 2.5
)

// Icon renders the tray icon for a state as PNG bytes: the zerock mark with a
// status badge in the corner. The template form (macOS) is the mark's
// silhouette in black with alpha, and the badge tells the state by shape since
// the menu bar recolours it: a solid dot when up, a ring while connecting, a
// dot with a bar when failed. The regular form colours the badge instead.
func Icon(state State, template bool) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	draw.Draw(img, img.Bounds(), loadMark(), image.Point{}, draw.Src)
	if template {
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2] = 0, 0, 0
		}
	}

	if state != StateStopped {
		tint := badgeColors[state]
		if template {
			tint = color.NRGBA{A: 0xff}
		}
		const samples = 4
		for y := 0; y < iconSize; y++ {
			for x := 0; x < iconSize; x++ {
				var cleared, filled int
				for sy := 0; sy < samples; sy++ {
					for sx := 0; sx < samples; sx++ {
						px := float64(x) + (float64(sx)+0.5)/samples
						py := float64(y) + (float64(sy)+0.5)/samples
						clear, fill := badgeAt(state, template, px, py)
						if clear {
							cleared++
						}
						if fill {
							filled++
						}
					}
				}
				if cleared == 0 {
					continue
				}
				// Fade the mark out under the gap, then paint the badge over.
				under := img.NRGBAAt(x, y)
				under.A = uint8(float64(under.A) * float64(samples*samples-cleared) / (samples * samples))
				img.SetNRGBA(x, y, blend(under, tint, float64(filled)/(samples*samples)))
			}
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// badgeAt reports whether a point lies inside the badge's clearance and inside
// its painted shape.
func badgeAt(state State, template bool, px, py float64) (clear, fill bool) {
	dx, dy := px-badgeCX, py-badgeCY
	d := math.Hypot(dx, dy)
	if d > badgeR+badgeGap {
		return false, false
	}
	if d > badgeR {
		return true, false
	}
	if !template {
		// A coloured disc says it all.
		return true, true
	}
	switch state {
	case StateConnecting, StateReconnecting:
		return true, d >= badgeR-3.5 // ring
	case StateFailed:
		return true, !(math.Abs(dy) <= 1.75 && math.Abs(dx) <= badgeR-3.5) // dot with a bar cut out
	default:
		return true, true // solid dot
	}
}

// blend paints tint over under with the given coverage.
func blend(under, tint color.NRGBA, coverage float64) color.NRGBA {
	if coverage <= 0 {
		return under
	}
	a := float64(tint.A) / 255 * coverage
	ua := float64(under.A) / 255 * (1 - a)
	outA := a + ua
	if outA == 0 {
		return color.NRGBA{}
	}
	mix := func(t, u uint8) uint8 {
		return uint8((float64(t)*a + float64(u)*ua) / outA)
	}
	return color.NRGBA{R: mix(tint.R, under.R), G: mix(tint.G, under.G), B: mix(tint.B, under.B), A: uint8(outA * 255)}
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
