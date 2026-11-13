package photos

import (
	"image"

	"golang.org/x/image/draw"
)

// toNRGBA converts any decoded image to a flat *image.NRGBA so the
// transform helpers operate on raw pixels (fast, no color-model
// dispatch per pixel). The common path — JPEG/PNG/WebP decode with
// normal orientation — is already a zero-origin NRGBA, in which case
// the 12 MP copy is skipped entirely (M7 perf: that copy was a fixed
// ~50 MB memmove on every upload).
func toNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok && n.Bounds().Min.X == 0 && n.Bounds().Min.Y == 0 {
		return n
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

func copyNRGBA(src *image.NRGBA) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	copy(dst.Pix, src.Pix)
	return dst
}

func copyRect(src *image.NRGBA, r image.Rectangle) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		srcRow := src.PixOffset(r.Min.X, r.Min.Y+y)
		dstRow := dst.PixOffset(0, y)
		copy(dst.Pix[dstRow:dstRow+r.Dx()*4], src.Pix[srcRow:srcRow+r.Dx()*4])
	}
	return dst
}

// drawBilinear scales src to fill dst (dst dimensions).
//
// M7 performance: the scaler is chosen by the scale factor.
//   - Large downscales (>1.5x on any axis — the 12 MP upload path):
//     ApproxBiLinear, a few times cheaper than CatmullRom; its
//     aliasing behaviour is not visible at frame-panel resolution
//     (documented in docs/performance.md).
//   - Small resizes and upscales: CatmullRom — ApproxBiLinear is
//     measurably SLOWER when upscaling (its rgba64 intermediates),
//     and upscale quality matters more there.
func drawBilinear(dst, src *image.NRGBA, w, h int) {
	sb := src.Bounds()
	var scaler draw.Scaler = draw.CatmullRom
	if float64(sb.Dx())/float64(w) > 1.5 || float64(sb.Dy())/float64(h) > 1.5 {
		scaler = draw.ApproxBiLinear
	}
	scaler.Scale(dst, image.Rect(0, 0, w, h), src, sb, draw.Over, nil)
}

// rotate90CW rotates a flat NRGBA image 90 degrees clockwise.
func rotate90CW(src *image.NRGBA) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			si := src.PixOffset(x, y)
			di := dst.PixOffset(h-1-y, x)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// flipH mirrors horizontally.
func flipH(src *image.NRGBA) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			si := src.PixOffset(x, y)
			di := dst.PixOffset(w-1-x, y)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// applyOrientation applies EXIF orientation (0/1 = identity, 2-8 per
// the EXIF 5.1 convention used by libvips/ImageMagick):
//
//	2: mirror horizontal
//	3: rotate 180
//	4: mirror vertical
//	5: mirror horizontal + rotate 270 CW
//	6: rotate 90 CW
//	7: mirror horizontal + rotate 90 CW
//	8: rotate 270 CW
func applyOrientation(src image.Image, orientation int) image.Image {
	n := toNRGBA(src)
	switch orientation {
	case 0, 1:
		return n
	case 2:
		return flipH(n)
	case 3:
		return rotate90CW(rotate90CW(n))
	case 4:
		return flipH(rotate90CW(rotate90CW(n))) // == vertical mirror
	case 5:
		return flipH(rotate90CW(rotate90CW(rotate90CW(n))))
	case 6:
		return rotate90CW(n)
	case 7:
		return flipH(rotate90CW(n))
	case 8:
		return rotate90CW(rotate90CW(rotate90CW(n)))
	default:
		return n
	}
}
