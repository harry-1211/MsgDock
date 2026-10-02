//go:build windows

package main

import (
	"image"
	"image/color"
	"log"

	"github.com/lxn/walk"
)

// statusDotImage draws an antialiased filled circle in the state color with a
// thin darker rim so the pastel palette still reads on a light taskbar. size
// is in device pixels; the result is fully transparent outside the circle.
func statusDotImage(size int, r, g, b uint8) *image.RGBA {
	if size < 8 {
		size = 8
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	center := float64(size) / 2
	outer := center - 0.5
	rim := float64(size) / 16
	if rim < 1 {
		rim = 1
	}
	inner := outer - rim
	rimColor := color.RGBA{R: uint8(float64(r) * 0.62), G: uint8(float64(g) * 0.62), B: uint8(float64(b) * 0.62), A: 255}
	fill := color.RGBA{R: r, G: g, B: b, A: 255}
	const samples = 4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var outerHits, innerHits int
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := float64(x) + (float64(sx)+0.5)/samples - center
					py := float64(y) + (float64(sy)+0.5)/samples - center
					d2 := px*px + py*py
					if d2 <= outer*outer {
						outerHits++
						if d2 <= inner*inner {
							innerHits++
						}
					}
				}
			}
			if outerHits == 0 {
				continue
			}
			total := float64(samples * samples)
			innerCover := float64(innerHits) / total
			rimCover := float64(outerHits-innerHits) / total
			alpha := innerCover + rimCover
			// Premultiplied blend of fill and rim weighted by coverage.
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(float64(fill.R)*innerCover + float64(rimColor.R)*rimCover),
				G: uint8(float64(fill.G)*innerCover + float64(rimColor.G)*rimCover),
				B: uint8(float64(fill.B)*innerCover + float64(rimColor.B)*rimCover),
				A: uint8(alpha * 255),
			})
		}
	}
	return img
}

// statusIconCache renders one icon per state and DPI on first use. Icons stay
// alive for the life of the UI because walk's icon cache and the tray keep
// handles to them; dispose() releases them once at shutdown.
type statusIconCache struct {
	icons map[statusIconKey]*walk.Icon
}

type statusIconKey struct {
	level syncLevel
	dpi   int
	size  int // logical (96 dpi) pixels
}

func (c *statusIconCache) icon(level syncLevel, logicalSize, dpi int) *walk.Icon {
	if c.icons == nil {
		c.icons = make(map[statusIconKey]*walk.Icon)
	}
	if dpi <= 0 {
		dpi = 96
	}
	key := statusIconKey{level: level, dpi: dpi, size: logicalSize}
	if icon, ok := c.icons[key]; ok {
		return icon
	}
	r, g, b := level.RGB()
	pixels := logicalSize * dpi / 96
	icon, err := walk.NewIconFromImageForDPI(statusDotImage(pixels, r, g, b), dpi)
	if err != nil {
		log.Printf("render status icon failed: %v", err)
		return nil
	}
	c.icons[key] = icon
	return icon
}

func (c *statusIconCache) dispose() {
	for key, icon := range c.icons {
		icon.Dispose()
		delete(c.icons, key)
	}
}
