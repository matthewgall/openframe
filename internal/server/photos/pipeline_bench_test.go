package photos

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// benchJPEG renders a gradient (not a solid fill: solids compress to
// almost nothing and would understate decode/encode cost) and encodes
// it at quality 90, like a camera JPEG.
func benchJPEG(b *testing.B, w, h int) []byte {
	b.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				uint8((x*7 + y) % 256), uint8((x + y*13) % 256), uint8((x * y) % 256), 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkProcessJPEG4032x3024 is the spec's target input: a full
// 12 MP smartphone photo run through validation, decode, EXIF pass
// and all four derivative encodes.
func BenchmarkProcessJPEG4032x3024(b *testing.B) {
	data := benchJPEG(b, 4032, 3024)
	opts := DefaultOptions(50<<20, 8000)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := Process(context.Background(), data, opts); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProcessJPEG1280x800 is a small (frame-resolution) photo —
// the fast path most uploads will actually be on home networks.
func BenchmarkProcessJPEG1280x800(b *testing.B) {
	data := benchJPEG(b, 1280, 800)
	opts := DefaultOptions(50<<20, 8000)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := Process(context.Background(), data, opts); err != nil {
			b.Fatal(err)
		}
	}
}
