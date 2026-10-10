package ocr

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif" // register the GIF decoder for image.Decode
	"image/jpeg"
	"image/png"
	"math"
)

// DefaultMaxImageDim caps the longest side of an image before it is sent to a
// vision model (0 disables downscaling). Providers downscale server-side too,
// but shipping a huge image wastes bandwidth and RAM on the Pi, so a generous
// cap keeps typical screenshots untouched while bounding the worst case.
const DefaultMaxImageDim = 2048

// downscaleImage returns a smaller re-encoded copy of data when its longest side
// exceeds maxDim. ok is false when data cannot be decoded with the stdlib (e.g.
// WebP) or is already small enough — the caller then sends the original bytes.
func downscaleImage(data []byte, maxDim int) (out []byte, outMime string, ok bool) {
	if maxDim <= 0 {
		return nil, "", false
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", false
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return nil, "", false
	}
	longest := w
	if h > longest {
		longest = h
	}
	scale := float64(maxDim) / float64(longest)
	nw := int(math.Round(float64(w) * scale))
	nh := int(math.Round(float64(h) * scale))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := resizeBox(img, nw, nh)

	var buf bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
			return nil, "", false
		}
		return buf.Bytes(), "image/jpeg", true
	}
	if err := png.Encode(&buf, dst); err != nil {
		return nil, "", false
	}
	return buf.Bytes(), "image/png", true
}

// resizeBox downscales src to nw x nh by averaging each destination pixel's
// source box (a simple box filter; good for shrinking text images). Only the
// stdlib is used — no golang.org/x/image dependency.
func resizeBox(src image.Image, nw, nh int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0 := sb.Min.Y + y*sh/nh
		y1 := sb.Min.Y + (y+1)*sh/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0 := sb.Min.X + x*sw/nw
			x1 := sb.Min.X + (x+1)*sw/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					b += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8((r / n) >> 8),
				G: uint8((g / n) >> 8),
				B: uint8((b / n) >> 8),
				A: uint8((a / n) >> 8),
			})
		}
	}
	return dst
}
