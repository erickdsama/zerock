package tray

import (
	"bytes"
	"embed"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"runtime"
	"sync"
)

// masks holds the mark's silhouette at each size a tray wants, pre-rendered
// from assets/icon.png so nothing scales at run time and no panel gets a
// bigger image than it can show without resampling. A flat silhouette is what
// survives 22 pixels; the facets and gradients of the full mark do not.
//
// Regenerate with, for each size:
//
//	convert icon.png -background none -gravity center -extent 563x563 \
//	  -alpha extract -morphology Dilate Disk:6 -filter Lanczos \
//	  -resize 22x22 -extent 22x22 -define png:color-type=0 mask-22.png
//
//go:embed assets/mask-*.png
var masks embed.FS

// iconSize is the pixel size each platform's tray shows an icon at: Linux
// panels 22, Windows 32 (scaled down at low DPI), macOS 18 points at 2x.
func iconSize() int {
	switch runtime.GOOS {
	case "windows":
		return 32
	case "darwin":
		return 36
	default:
		return 22
	}
}

// markTint is the silhouette colour on Linux and Windows: a mid blue from the
// mark's arrows that reads on both dark and light panels. macOS template icons
// are black and recoloured by the menu bar.
var markTint = color.NRGBA{R: 0x5b, G: 0x8a, B: 0xb8, A: 0xff}

// badgeColors are the status badge colours per state. Stopped has no badge.
var badgeColors = map[State]color.NRGBA{
	StateUp:           {R: 0x2f, G: 0xa8, B: 0x4f, A: 0xff},
	StateConnecting:   {R: 0xe0, G: 0xa0, B: 0x20, A: 0xff},
	StateReconnecting: {R: 0xe0, G: 0xa0, B: 0x20, A: 0xff},
	StateFailed:       {R: 0xd9, G: 0x48, B: 0x3b, A: 0xff},
}

var (
	maskMu    sync.Mutex
	maskCache = map[int]*image.Gray{}
)

// loadMask returns the silhouette at size, decoding it once.
func loadMask(size int) *image.Gray {
	maskMu.Lock()
	defer maskMu.Unlock()
	if m, ok := maskCache[size]; ok {
		return m
	}
	raw, err := masks.ReadFile(fmt.Sprintf("assets/mask-%d.png", size))
	if err != nil {
		panic("tray: no embedded mask for size " + fmt.Sprint(size))
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		panic("tray: embedded mask is not a PNG: " + err.Error())
	}
	m := image.NewGray(image.Rect(0, 0, size, size))
	draw.Draw(m, m.Bounds(), img, img.Bounds().Min, draw.Src)
	maskCache[size] = m
	return m
}

// Icon renders the tray icon for a state as PNG bytes: the mark's silhouette
// with a status badge in the corner. The template form (macOS) is black with
// alpha, and the badge tells the state by shape since the menu bar recolours
// it: a solid dot when up, a ring while connecting, a dot with a bar when
// failed. The regular form colours the badge instead.
func Icon(state State, template bool) []byte {
	return iconAt(iconSize(), state, template)
}

func iconAt(size int, state State, template bool) []byte {
	mask := loadMask(size)
	tint := markTint
	if template {
		tint = color.NRGBA{A: 0xff}
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if a := mask.GrayAt(x, y).Y; a != 0 {
				c := tint
				c.A = a
				img.SetNRGBA(x, y, c)
			}
		}
	}

	if state != StateStopped {
		badge := badgeColors[state]
		if template {
			badge = color.NRGBA{A: 0xff}
		}
		g := newBadgeGeometry(size)
		const samples = 4
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				var cleared, filled int
				for sy := 0; sy < samples; sy++ {
					for sx := 0; sx < samples; sx++ {
						px := float64(x) + (float64(sx)+0.5)/samples
						py := float64(y) + (float64(sy)+0.5)/samples
						clear, fill := g.at(state, template, px, py)
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
				// Fade the mark out under the badge's gap, then paint over.
				under := img.NRGBAAt(x, y)
				under.A = uint8(float64(under.A) * float64(samples*samples-cleared) / (samples * samples))
				img.SetNRGBA(x, y, blend(under, badge, float64(filled)/(samples*samples)))
			}
		}
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// badgeGeometry is the badge's placement for an icon size: bottom-right, a
// little under a fifth of the icon across, with a transparent gap so it
// separates from the mark.
type badgeGeometry struct{ cx, cy, r, gap, stroke float64 }

func newBadgeGeometry(size int) badgeGeometry {
	s := float64(size)
	r := s * 0.19
	return badgeGeometry{cx: s - r - 0.5, cy: s - r - 0.5, r: r, gap: s * 0.06, stroke: math.Max(1.25, s*0.06)}
}

// at reports whether a point lies inside the badge's clearance and inside its
// painted shape.
func (g badgeGeometry) at(state State, template bool, px, py float64) (clear, fill bool) {
	dx, dy := px-g.cx, py-g.cy
	d := math.Hypot(dx, dy)
	if d > g.r+g.gap {
		return false, false
	}
	if d > g.r {
		return true, false
	}
	if !template {
		return true, true // a coloured disc says it all
	}
	switch state {
	case StateConnecting, StateReconnecting:
		return true, d >= g.r-g.stroke // ring
	case StateFailed:
		return true, !(math.Abs(dy) <= g.stroke/2 && math.Abs(dx) <= g.r-g.stroke) // dot with a bar cut out
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
		regular = icoWrap(regular, iconSize())
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
