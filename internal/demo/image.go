package demo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
)

// point is a screen coordinate of the isometric projection.
type point struct{ X, Y int }

// project maps a point of the isometric coordinate system to screen
// coordinates: a runs to the lower right, b to the lower left, c upwards. The
// 2:1 ratio is the usual pixel-art isometry.
func project(origin point, a, b, c int) point {
	return point{X: origin.X + a - b, Y: origin.Y + (a+b)/2 - c}
}

// fillPoly fills a convex polygon using a scanline pass.
func fillPoly(img *image.RGBA, pts []point, c color.RGBA) {
	if len(pts) < 3 {
		return
	}
	minY, maxY := pts[0].Y, pts[0].Y
	for _, p := range pts[1:] {
		minY, maxY = min(minY, p.Y), max(maxY, p.Y)
	}
	bounds := img.Bounds()
	for y := max(minY, bounds.Min.Y); y <= min(maxY, bounds.Max.Y-1); y++ {
		lo, hi, found := 0, 0, false
		for i := range pts {
			p, q := pts[i], pts[(i+1)%len(pts)]
			if p.Y == q.Y || y < min(p.Y, q.Y) || y >= max(p.Y, q.Y) {
				continue
			}
			x := p.X + (y-p.Y)*(q.X-p.X)/(q.Y-p.Y)
			if !found {
				lo, hi, found = x, x, true
				continue
			}
			lo, hi = min(lo, x), max(hi, x)
		}
		if !found {
			continue
		}
		for x := max(lo, bounds.Min.X); x <= min(hi, bounds.Max.X-1); x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

// shade lightens or darkens a color by the given factor (100 = unchanged).
func shade(c color.RGBA, percent int) color.RGBA {
	clamp := func(v int) uint8 {
		return uint8(min(max(v, 0), 255)) //nolint:gosec // clamped to 0..255
	}
	return color.RGBA{
		R: clamp(int(c.R) * percent / 100),
		G: clamp(int(c.G) * percent / 100),
		B: clamp(int(c.B) * percent / 100),
		A: c.A,
	}
}

// box draws a cuboid with its top, right and left face.
func box(img *image.RGBA, origin point, c0, w, d, h int, face color.RGBA) {
	fillPoly(img, []point{
		project(origin, w, 0, c0),
		project(origin, w, d, c0),
		project(origin, w, d, c0+h),
		project(origin, w, 0, c0+h),
	}, shade(face, 62))
	fillPoly(img, []point{
		project(origin, 0, d, c0),
		project(origin, w, d, c0),
		project(origin, w, d, c0+h),
		project(origin, 0, d, c0+h),
	}, shade(face, 84))
	fillPoly(img, []point{
		project(origin, 0, 0, c0+h),
		project(origin, w, 0, c0+h),
		project(origin, w, d, c0+h),
		project(origin, 0, d, c0+h),
	}, face)
}

// renderIllustration draws the artwork the demo shows instead of a real image
// model result: an isometric server rack in the colors of the interface.
// accent shifts the highlight so different prompts yield different images.
func renderIllustration(width, height int, accent color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// Background gradient, matching the dark UI theme.
	for y := 0; y < height; y++ {
		v := uint8(0x13 + y*0x10/height) //nolint:gosec // stays below 0x30
		row := color.RGBA{R: v, G: v, B: v + 6, A: 0xff}
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, row)
		}
	}

	// Geometry, scaled to the shorter edge so the rack always fits.
	scale := min(width, height)
	unitW := scale * 42 / 100
	unitD := scale * 30 / 100
	unitH := scale * 8 / 100
	gap := scale * 2 / 100
	const units = 6
	stack := units*(unitH+gap) - gap

	origin := point{
		X: width/2 + (unitD-unitW)/2,
		Y: height/2 + (stack-(unitW+unitD)/2)/2,
	}

	// Floor plate.
	pad := scale * 10 / 100
	fillPoly(img, []point{
		project(origin, -pad, -pad, 0),
		project(origin, unitW+pad, -pad, 0),
		project(origin, unitW+pad, unitD+pad, 0),
		project(origin, -pad, unitD+pad, 0),
	}, color.RGBA{R: 0x22, G: 0x26, B: 0x2c, A: 0xff})

	// The rack units, bottom to top.
	for i := 0; i < units; i++ {
		base := i * (unitH + gap)
		box(img, origin, base, unitW, unitD, unitH,
			shade(color.RGBA{R: 0x3c, G: 0x42, B: 0x4b, A: 0xff}, 100-i*5))

		// Status lights on the left face.
		lightW, lightH := max(scale/80, 2), max(unitH/4, 2)
		for l := 0; l < 4; l++ {
			a := unitW/8 + l*unitW/5
			light := accent
			if l == 2 {
				light = shade(accent, 50)
			}
			fillPoly(img, []point{
				project(origin, a, unitD, base+unitH/3),
				project(origin, a+lightW, unitD, base+unitH/3),
				project(origin, a+lightW, unitD, base+unitH/3+lightH),
				project(origin, a, unitD, base+unitH/3+lightH),
			}, light)
		}
	}

	// Accent plate closing the stack.
	box(img, origin, stack+gap, unitW, unitD, max(unitH/4, 2), accent)
	return img
}

// accentFor derives a stable accent color from a prompt so repeated
// generations differ while a given prompt always yields the same image.
func accentFor(prompt string) color.RGBA {
	var h uint32 = 2166136261
	for _, r := range strings.ToLower(prompt) {
		h = (h ^ uint32(r)) * 16777619
	}
	palette := []color.RGBA{
		{R: 0x10, G: 0xa3, B: 0x7f, A: 0xff}, // UI accent
		{R: 0x3d, G: 0x9b, B: 0xe9, A: 0xff},
		{R: 0x2f, G: 0xc2, B: 0xb0, A: 0xff},
		{R: 0x6f, G: 0xb8, B: 0x4a, A: 0xff},
	}
	return palette[h%uint32(len(palette))]
}

// encodeImage renders the illustration in the requested format. Everything
// except JPEG is served as PNG, which is what the image models return.
func encodeImage(img *image.RGBA, format string) ([]byte, string, error) {
	var buf bytes.Buffer
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "jpeg" || format == "jpg" {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/jpeg", nil
	}
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/png", nil
}
