package photos

import (
	"image"

	"golang.org/x/image/draw"
)

// toNRGBA converts any decoded image to a flat *image.NRGBA copy so the
// transform helpers operate on raw pixels (fast, no color-model
// dispatch per pixel).
func toNRGBA(src image.Image) *image.NRGBA {
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

// drawBilinear scales src to fill dst (dst dimensions) with a Catmull
// Rom scaler.
func drawBilinear(dst, src *image.NRGBA, w, h int) {
	draw.CatmullRom.Scale(dst, image.Rect(0, 0, w, h), src, src.Bounds(), draw.Over, nil)
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
