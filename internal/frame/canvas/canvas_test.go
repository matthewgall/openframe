package canvas

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestNRGBAMemoryLayout(t *testing.T) {
	// The DRM display driver's XRGB8888 conversion depends on this
	// byte order. Go < 1.25 stored NRGBA as [A,R,G,B]; Go >= 1.25
	// stores it as [R,G,B,A]. If this ever changes again, this test
	// fails first instead of a blue-tinted photo frame.
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 0xAB, G: 0xCD, B: 0xEF, A: 0x12})
	want := []byte{0xAB, 0xCD, 0xEF, 0x12}
	for i, b := range want {
		if img.Pix[i] != b {
			t.Fatalf("image.NRGBA Pix byte %d = %02x, want %02x — NRGBA layout is not [R,G,B,A]; update writeXRGB8888 in the DRM driver", i, img.Pix[i], b)
		}
	}
}

func TestFillAndRect(t *testing.T) {
	c := New(32, 16)
	c.Fill(color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	if p := c.At(5, 7); p != (color.NRGBA{R: 10, G: 20, B: 30, A: 255}) {
		t.Errorf("fill pixel = %v", p)
	}
	c.FillRect(4, 4, 8, 4, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	if p := c.At(5, 5); p != (color.NRGBA{R: 255, G: 0, B: 0, A: 255}) {
		t.Errorf("rect pixel = %v", p)
	}
	// Clipped at edges: a rect straddling the corner keeps only its
	// inside portion.
	c.FillRect(-3, -3, 8, 8, color.NRGBA{R: 0, G: 255, B: 0, A: 255})
	if p := c.At(0, 0); p != (color.NRGBA{R: 0, G: 255, B: 0, A: 255}) {
		t.Errorf("clip pixel = %v", p)
	}
	if p := c.At(6, 0); p != (color.NRGBA{R: 10, G: 20, B: 30, A: 255}) {
		t.Errorf("outside clip = %v", p)
	}
	// Alpha blend: 50% white over black.
	c.Fill(color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	c.FillRect(0, 0, 4, 4, color.NRGBA{R: 255, G: 255, B: 255, A: 128})
	p := c.At(1, 1)
	if p.R < 100 || p.R > 160 {
		t.Errorf("blend R = %d, want ~128", p.R)
	}
}

func TestFontDrawsVisiblePixels(t *testing.T) {
	c := New(200, 60)
	c.Fill(color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	f := c.Font(3)
	w := f.DrawText(c.img, 10, 5, "Hi", color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	if w != len("Hi")*f.CellW() {
		t.Errorf("advance = %d, want %d", w, len("Hi")*f.CellW())
	}
	// "H" has a solid vertical stroke: some dark pixel must exist in
	// the first cell column band.
	found := false
	for y := 0; y < f.CellH() && !found; y++ {
		for x := 0; x < f.CellW() && !found; x++ {
			if c.At(10+x, 5+y).R < 128 {
				found = true
			}
		}
	}
	if !found {
		t.Error("no dark pixels drawn for 'H'")
	}
	// Width math.
	if got := f.Width("abc"); got != 3*f.CellW() {
		t.Errorf("Width(abc) = %d, want %d", got, 3*f.CellW())
	}
	// Space advances but draws nothing: canvas row below the cell is
	// still white.
	c2 := New(200, 60)
	c2.Fill(color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	c2.Font(3).DrawText(c2.img, 10, 5, "  ", color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	for y := 0; y < 60; y++ {
		for x := 0; x < 200; x++ {
			if p := c2.At(x, y); p.R != 255 {
				t.Fatalf("space drew a pixel at (%d,%d)", x, y)
			}
		}
	}
}

// solidColorImage makes a w x h NRGBA filled with col.
// toNRGBA normalises any color.Color to NRGBA for comparison.
func toNRGBA(c color.Color) color.NRGBA {
	if n, ok := c.(color.NRGBA); ok {
		return n
	}
	if n, ok := c.(color.RGBA); ok {
		return color.NRGBA{R: n.R, G: n.G, B: n.B, A: n.A}
	}
	r, g, b, a := c.RGBA()
	return color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

func solidColorImage(w, h int, col color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, col)
		}
	}
	return img
}

func TestFitContainLetterboxes(t *testing.T) {
	c := New(10, 10)
	// 20x10 source into 10x10: scale 0.5 -> 10x5, centred vertically.
	src := solidColorImage(20, 10, color.NRGBA{R: 200, G: 0, B: 0, A: 255})
	out := c.Fit(src, 10, 10, FitContain)
	// Middle row is the photo.
	if p := out.At(5, 5); p != (color.NRGBA{R: 200, G: 0, B: 0, A: 255}) {
		t.Errorf("centre = %v", p)
	}
	// Top/bottom rows are black bars.
	if p := out.At(5, 0); p != (color.NRGBA{R: 0, G: 0, B: 0, A: 255}) {
		t.Errorf("letterbox = %v", p)
	}
}

func TestFitCoverCropsCentre(t *testing.T) {
	c := New(10, 10)
	// 10x20 source into 10x10: scale 1, centre 10x10 band kept.
	src := solidColorImage(10, 20, color.NRGBA{R: 0, G: 200, B: 0, A: 255})
	out := c.Fit(src, 10, 10, FitCover)
	if p := out.At(5, 5); p != (color.NRGBA{R: 0, G: 200, B: 0, A: 255}) {
		t.Errorf("cover centre = %v", p)
	}
	// Aspect is preserved: a 4x8 source into 10x10 scales to
	// 10x20 and crops vertically.
	src2 := solidColorImage(4, 8, color.NRGBA{R: 0, G: 0, B: 200, A: 255})
	out2 := c.Fit(src2, 10, 10, FitCover)
	if p := out2.At(9, 9); p != (color.NRGBA{R: 0, G: 0, B: 200, A: 255}) {
		t.Errorf("cover corner = %v", p)
	}
}

func TestFitCropFillsExactly(t *testing.T) {
	c := New(10, 10)
	src := solidColorImage(3, 7, color.NRGBA{R: 100, G: 100, B: 100, A: 255})
	out := c.Fit(src, 10, 10, FitCrop)
	// Every corner is the source colour (stretched to fill).
	for _, pt := range []image.Point{{0, 0}, {9, 0}, {0, 9}, {9, 9}, {5, 5}} {
		if got := out.At(pt.X, pt.Y).(color.NRGBA); got != (color.NRGBA{R: 100, G: 100, B: 100, A: 255}) {
			t.Errorf("crop at %v = %v", pt, got)
		}
	}
}

func TestLoadAndFitPhoto(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "img.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, solidColorImage(20, 20, color.NRGBA{R: 1, G: 2, B: 3, A: 255})); err != nil {
		t.Fatal(err)
	}
	f.Close()

	c := New(10, 10)
	out, err := c.FitPhoto(path, 10, 10, FitCrop)
	if err != nil {
		t.Fatal(err)
	}
	if p := out.At(5, 5).(color.NRGBA); p.R != 1 || p.G != 2 || p.B != 3 {
		t.Errorf("photo pixel = %v", p)
	}
	// Cache: second call returns the same image pointer.
	src2, err := c.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	src1, _ := c.Load(path)
	if src1 != src2 {
		t.Error("photo not cached")
	}
	if _, err := c.Load(filepath.Join(dir, "missing.png")); err == nil {
		t.Error("missing file: expected error")
	}
}

func TestCrossfadeEndpointsAndMidpoint(t *testing.T) {
	w := 4
	base := solidColorImage(w, w, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	top := solidColorImage(w, w, color.NRGBA{R: 200, G: 0, B: 0, A: 255})
	dst := solidColorImage(w, w, color.NRGBA{R: 99, G: 99, B: 99, A: 255})

	BlendCrossfade(dst, base, top, 0)
	if p := dst.At(0, 0); p != (color.NRGBA{R: 0, G: 0, B: 0, A: 255}) {
		t.Errorf("t=0 = %v", p)
	}
	BlendCrossfade(dst, base, top, 1)
	if p := dst.At(0, 0); p != (color.NRGBA{R: 200, G: 0, B: 0, A: 255}) {
		t.Errorf("t=1 = %v", p)
	}
	BlendCrossfade(dst, base, top, 0.5)
	if p := dst.At(0, 0).(color.NRGBA); p.R < 90 || p.R > 110 {
		t.Errorf("t=0.5 R = %d, want ~100", p.R)
	}
}

func TestSlideFrame(t *testing.T) {
	w := 10
	base := solidColorImage(w, 2, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	top := solidColorImage(w, 2, color.NRGBA{R: 0, G: 0, B: 255, A: 255})
	dst := solidColorImage(w, 2, color.NRGBA{R: 9, G: 9, B: 9, A: 255})

	SlideFrame(dst, base, top, 0)
	if p := dst.At(0, 0); p != (color.NRGBA{R: 255, G: 0, B: 0, A: 255}) {
		t.Errorf("t=0 = %v", p)
	}
	SlideFrame(dst, base, top, 1)
	if p := dst.At(9, 0); p != (color.NRGBA{R: 0, G: 0, B: 255, A: 255}) {
		t.Errorf("t=1 = %v", p)
	}
	// Midpoint: base keeps its left half, top has slid in to occupy
	// the right half; they meet at the centre with no gap.
	SlideFrame(dst, base, top, 0.5)
	if p := dst.At(0, 0); p != (color.NRGBA{R: 255, G: 0, B: 0, A: 255}) {
		t.Errorf("mid left = %v, want base (red)", p)
	}
	if p := dst.At(9, 0); p != (color.NRGBA{R: 0, G: 0, B: 255, A: 255}) {
		t.Errorf("mid right = %v, want top (blue)", p)
	}
	// The meeting point belongs to the incoming image (top).
	if p := dst.At(5, 0); p != (color.NRGBA{R: 0, G: 0, B: 255, A: 255}) {
		t.Errorf("mid meet = %v, want top (blue)", p)
	}
}

func TestSnapshotPNGRoundTrip(t *testing.T) {
	c := New(8, 4)
	c.Fill(color.NRGBA{R: 12, G: 34, B: 56, A: 255})
	path := filepath.Join(t.TempDir(), "snap.png")
	if err := c.SnapshotPNG(path); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 8 || img.Bounds().Dy() != 4 {
		t.Errorf("snapshot size = %v", img.Bounds())
	}
	// PNG decode yields RGBA; compare in a normalised form.
	want := toNRGBA(color.RGBA{12, 34, 56, 255})
	if got := toNRGBA(img.At(3, 2)); got != want {
		t.Errorf("snapshot pixel = %v, want %v", got, want)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}
