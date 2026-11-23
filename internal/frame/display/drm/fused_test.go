//go:build linux

package drm

import (
	"testing"
)

func TestWriteBlendXRGB8888(t *testing.T) {
	w, h, pitch := 2, 2, 12
	base := []byte{
		0xFF, 0x00, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF,
		0x00, 0x00, 0xFF, 0xFF, 0x80, 0x80, 0x80, 0xFF,
	}
	top := []byte{
		0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00, 0xFF,
		0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00, 0xFF,
	}
	dst := make([]byte, h*pitch)
	writeBlendXRGB8888(dst, base, top, w, h, pitch, 256) // t=1 -> top
	// At p=256 the blend is exactly top, channel-swapped.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			si := (y*w + x) * 4
			di := y*pitch + x*4
			if dst[di] != top[si+2] || dst[di+1] != top[si+1] || dst[di+2] != top[si] || dst[di+3] != 0 {
				t.Fatalf("p=256 pixel (%d,%d) = % x, want swapped top % x", x, y, dst[di:di+4], []byte{top[si+2], top[si+1], top[si], 0})
			}
		}
	}
	// At p=0 it is exactly base.
	writeBlendXRGB8888(dst, base, top, w, h, pitch, 0)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			si := (y*w + x) * 4
			di := y*pitch + x*4
			if dst[di] != base[si+2] || dst[di+1] != base[si+1] || dst[di+2] != base[si] {
				t.Fatalf("p=0 pixel (%d,%d) = % x, want swapped base", x, y, dst[di:di+4])
			}
		}
	}
	// Midpoint: opaque red over black -> ~half red in the R slot.
	writeBlendXRGB8888(dst, base, top, w, h, pitch, 128)
	if got := dst[2]; got < 120 || got > 130 { // R byte of pixel (0,0)
		t.Fatalf("midpoint R = %d, want ~127", got)
	}
}

func TestWriteSlideXRGB8888(t *testing.T) {
	w, h, pitch, off := 10, 2, 40, 3
	base := make([]byte, w*h*4)
	top := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			base[i] = 255 // red
			base[i+3] = 255
			top[i] = 0
			top[i+1] = 0
			top[i+2] = 255 // blue
			top[i+3] = 255
		}
	}
	dst := make([]byte, h*pitch)
	writeSlideXRGB8888(dst, base, top, w, h, pitch, off)
	// dst[x] = base[x+off] for x < off (base-only band), red.
	// dst[x] = top[x-off] for x >= off, blue.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			di := y*pitch + x*4
			red := dst[di+2] == 255 && dst[di] == 0
			blue := dst[di] == 255 && dst[di+2] == 0
			if x < off && !red {
				t.Fatalf("y=%d x=%d want base red, got % x", y, x, dst[di:di+4])
			}
			if x >= off && !blue {
				t.Fatalf("y=%d x=%d want top blue, got % x", y, x, dst[di:di+4])
			}
		}
	}
	// Late half (off > w/2): a transparent gap sits between the bands.
	off = 7
	writeSlideXRGB8888(dst, base, top, w, h, pitch, off)
	for x := 0; x < w; x++ {
		di := x * 4
		switch {
		case x < w-off: // base band [0, 3)
			if dst[di+2] != 255 {
				t.Fatalf("gap case: x=%d want base red, got % x", x, dst[di:di+4])
			}
		case x < off: // gap [3, 7)
			if dst[di]|dst[di+1]|dst[di+2]|dst[di+3] != 0 {
				t.Fatalf("gap: x=%d want transparent black, got % x", x, dst[di:di+4])
			}
		default: // top band [7, 10)
			if dst[di] != 255 {
				t.Fatalf("gap case: x=%d want top blue, got % x", x, dst[di:di+4])
			}
		}
	}
}

func BenchmarkWriteBlend(b *testing.B) {
	w, h, pitch := 1920, 1200, 1920*4
	base := make([]byte, w*h*4)
	top := make([]byte, w*h*4)
	dst := make([]byte, h*pitch)
	base[0] = 12
	top[0] = 3
	b.SetBytes(int64(len(dst)))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		writeBlendXRGB8888(dst, base, top, w, h, pitch, 128)
	}
}

func BenchmarkWriteSlide(b *testing.B) {
	w, h, pitch := 1920, 1200, 1920*4
	base := make([]byte, w*h*4)
	top := make([]byte, w*h*4)
	dst := make([]byte, h*pitch)
	base[0] = 12
	top[0] = 3
	b.SetBytes(int64(len(dst)))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		writeSlideXRGB8888(dst, base, top, w, h, pitch, w/3)
	}
}

func BenchmarkWriteBlendHalf(b *testing.B) {
	// 960x600 = the half-res crossfade path (HVS upscales on scanout).
	w, h, pitch := 960, 600, 960*4
	base := make([]byte, w*h*4)
	top := make([]byte, w*h*4)
	dst := make([]byte, h*pitch)
	base[0] = 12
	top[0] = 3
	b.SetBytes(int64(len(dst)))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		writeBlendXRGB8888(dst, base, top, w, h, pitch, 128)
	}
}
