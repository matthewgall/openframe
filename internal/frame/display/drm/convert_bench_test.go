//go:build linux

package drm

import "testing"

func BenchmarkWriteXRGB8888(b *testing.B) {
	w, h, pitch := 1920, 1200, 1920*4
	src := make([]byte, w*h*4)
	src[0] = 12
	dst := make([]byte, h*pitch)
	b.SetBytes(int64(len(dst)))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		writeXRGB8888(dst, src, w, h, pitch)
	}
}
