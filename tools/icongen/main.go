// Command icongen renders the AIO Agent icons (app icon, tray icons, .ico)
// into cmd/desktop/assets. Run from the repo root: go run ./tools/icongen
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

const out = "cmd/desktop/assets"

func main() {
	must(writePNG("appicon.png", render(1024, styleApp)))
	must(writePNG("tray.png", render(64, styleApp)))
	must(writePNG("tray-template.png", render(44, styleTemplate)))
	must(writePNG("tray-template-off.png", render(44, styleTemplateOff)))
	must(writeICO("icon.ico", 16, 24, 32, 48, 64, 128, 256))
}

type style int

const (
	styleApp style = iota
	styleTemplate
	styleTemplateOff
)

// render draws the icon at size×size using signed distance fields so edges are
// anti-aliased at any resolution. Coordinates are normalized to [0,1].
func render(size int, st style) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	px := 1.0 / float64(size) // one pixel in normalized units
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			u, v := (float64(x)+0.5)*px, (float64(y)+0.5)*px
			glyph := cover(glyphDist(u, v, st != styleApp), px)

			if st != styleApp {
				a := glyph
				if st == styleTemplateOff {
					a *= 0.45
				}
				img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, uint8(a * 255)})
				continue
			}

			// macOS-style squircle-ish plate with ~10% margin.
			plate := cover(roundRect(u-0.5, v-0.5, 0.40, 0.40, 0.09), px)
			if plate == 0 {
				continue
			}
			// Diagonal indigo→violet gradient.
			t := (u + v) / 2
			r := lerp(0x4f, 0x9b, t)
			g := lerp(0x46, 0x5c, t)
			b := lerp(0xe5, 0xf6, t)
			// Composite white glyph over the plate.
			r = lerp(r, 255, glyph)
			g = lerp(g, 255, glyph)
			b = lerp(b, 255, glyph)
			img.SetNRGBA(x, y, color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(plate * 255)})
		}
	}
	return img
}

// glyphDist is the distance to a ">_" terminal prompt glyph. The tray variant
// fills more of the canvas since there is no plate around it.
func glyphDist(u, v float64, tray bool) float64 {
	s, ox, oy := 1.0, 0.0, 0.0
	if tray {
		s, ox, oy = 1.45, -0.245, -0.225 // scale up and recenter
	}
	u, v = u/s-ox/s, v/s-oy/s
	w := 0.042 // stroke half-width
	d := capsule(u, v, 0.30, 0.34, 0.45, 0.50, w)
	d = math.Min(d, capsule(u, v, 0.45, 0.50, 0.30, 0.66, w))
	d = math.Min(d, capsule(u, v, 0.52, 0.66, 0.70, 0.66, w))
	return d * s
}

func capsule(px, py, ax, ay, bx, by, r float64) float64 {
	pax, pay, bax, bay := px-ax, py-ay, bx-ax, by-ay
	h := clamp((pax*bax+pay*bay)/(bax*bax+bay*bay), 0, 1)
	return math.Hypot(pax-bax*h, pay-bay*h) - r
}

func roundRect(px, py, hw, hh, r float64) float64 {
	qx, qy := math.Abs(px)-hw+r, math.Abs(py)-hh+r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

// cover maps a signed distance to pixel coverage with a 1px soft edge.
func cover(d, px float64) float64 { return clamp(0.5-d/px, 0, 1) }

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

func writePNG(name string, img image.Image) error {
	f, err := os.Create(filepath.Join(out, name))
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// writeICO writes a Windows .ico with PNG-compressed entries (Vista+).
func writeICO(name string, sizes ...int) error {
	var imgs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, render(s, styleApp)); err != nil {
			return err
		}
		imgs = append(imgs, buf.Bytes())
	}
	var buf bytes.Buffer
	le := binary.LittleEndian
	binary.Write(&buf, le, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := uint8(s)
		if s >= 256 {
			dim = 0 // 0 means 256
		}
		binary.Write(&buf, le, [4]uint8{dim, dim, 0, 0})
		binary.Write(&buf, le, [2]uint16{1, 32})
		binary.Write(&buf, le, [2]uint32{uint32(len(imgs[i])), uint32(offset)})
		offset += len(imgs[i])
	}
	for _, b := range imgs {
		buf.Write(b)
	}
	return os.WriteFile(filepath.Join(out, name), buf.Bytes(), 0o644)
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
