package canvas

import (
	"image"
	"testing"
)

func BenchmarkBlendCrossfade(b *testing.B) {
	base := image.NewNRGBA(image.Rect(0, 0, 1920, 1200))
	top := image.NewNRGBA(image.Rect(0, 0, 1920, 1200))
	dst := image.NewNRGBA(image.Rect(0, 0, 1920, 1200))
	for i := range base.Pix {
		base.Pix[i] = byte(i)
		top.Pix[i] = byte(i * 3)
	}
	b.SetBytes(int64(len(dst.Pix)))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		BlendCrossfade(dst, base, top, 0.5)
	}
}

func BenchmarkSlideFrame(b *testing.B) {
	base := image.NewNRGBA(image.Rect(0, 0, 1920, 1200))
	top := image.NewNRGBA(image.Rect(0, 0, 1920, 1200))
	dst := image.NewNRGBA(image.Rect(0, 0, 1920, 1200))
	for i := range base.Pix {
		base.Pix[i] = byte(i)
		top.Pix[i] = byte(i * 3)
	}
	b.SetBytes(int64(len(dst.Pix)))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		SlideFrame(dst, base, top, 0.37)
	}
}
