package photos

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// solidJPEG renders a w×h gradient and encodes it as JPEG.
func solidJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buf.Bytes()
}

// jpegWithEXIFOrientation splices a minimal EXIF APP1 segment (with the
// given orientation, 1-8) into a real JPEG right after the SOI marker.
func jpegWithEXIFOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	jpg := solidJPEG(t, w, h)

	// TIFF (little-endian) with a single IFD entry: Orientation (0x0112).
	tiff := make([]byte, 0, 8+2+12+4)
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x4949) // "II"
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x002A) // TIFF magic
	tiff = binary.LittleEndian.AppendUint32(tiff, 8)      // IFD offset
	tiff = binary.LittleEndian.AppendUint16(tiff, 1)      // entry count
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112) // Orientation
	tiff = binary.LittleEndian.AppendUint16(tiff, 3)      // SHORT
	tiff = binary.LittleEndian.AppendUint32(tiff, 1)      // count
	tiff = binary.LittleEndian.AppendUint16(tiff, uint16(orientation))
	tiff = append(tiff, 0, 0)                        // pad to 4 bytes
	tiff = binary.LittleEndian.AppendUint32(tiff, 0) // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff...)
	app1 := make([]byte, 0, 4+len(payload))
	app1 = append(app1, 0xFF, 0xE1)
	app1 = binary.BigEndian.AppendUint16(app1, uint16(2+len(payload)))
	app1 = append(app1, payload...)

	// Rebuild: SOI + APP1 + rest of the original JPEG (from its first
	// marker after SOI).
	out := make([]byte, 0, len(jpg)+len(app1))
	out = append(out, 0xFF, 0xD8)
	out = append(out, app1...)
	out = append(out, jpg[2:]...)
	return out
}

func TestProcessJPEG(t *testing.T) {
	data := solidJPEG(t, 1600, 1000)
	opts := DefaultOptions(1<<30, 8000)
	md, original, derivs, err := Process(context.Background(), data, opts)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if md.MIME != MIMEJPEG {
		t.Fatalf("mime: %s", md.MIME)
	}
	if md.Width != 1600 || md.Height != 1000 {
		t.Fatalf("dimensions: %dx%d", md.Width, md.Height)
	}
	if len(original) != len(data) {
		t.Fatal("original bytes changed")
	}
	if md.Hash == "" || len(md.Hash) != 64 {
		t.Fatalf("bad hash: %q", md.Hash)
	}
	for _, v := range []string{"display", "thumb", "portrait", "landscape"} {
		b, ok := derivs[v]
		if !ok || len(b) == 0 {
			t.Fatalf("missing derivative %s", v)
		}
		if b[0] != 0xFF || b[1] != 0xD8 {
			t.Fatalf("derivative %s is not JPEG", v)
		}
	}
	// Thumbnail is a 384px center crop.
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(derivs["thumb"]))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 384 || cfg.Height != 384 {
		t.Fatalf("thumb dims: %dx%d", cfg.Width, cfg.Height)
	}
}

func TestProcessPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 800, 600))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	md, _, derivs, err := Process(context.Background(), buf.Bytes(), DefaultOptions(1<<30, 8000))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if md.MIME != MIMEPNG || md.Width != 800 || md.Height != 600 {
		t.Fatalf("png meta: %+v", md)
	}
	if len(derivs["display"]) == 0 {
		t.Fatal("no display derivative")
	}
}

func TestProcessRejectsHEIC(t *testing.T) {
	heic := append([]byte("\x00\x00\x00\x18ftypheic"), make([]byte, 8)...)
	_, _, _, err := Process(context.Background(), heic, DefaultOptions(1<<30, 8000))
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("expected unsupported format, got: %v", err)
	}
}

func TestProcessRejectsLosslessWebP(t *testing.T) {
	webp := []byte("RIFF\x12\x00\x00\x00WEBPVP8L\x00\x00\x00\x00")
	_, _, _, err := Process(context.Background(), webp, DefaultOptions(1<<30, 8000))
	if !errors.Is(err, ErrWebPNotLossy) {
		t.Fatalf("expected lossless-webp rejection, got: %v", err)
	}
}

func TestProcessRejectsGarbage(t *testing.T) {
	_, _, _, err := Process(context.Background(), []byte("definitely not an image, really not"), DefaultOptions(1<<30, 8000))
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("expected unsupported format, got: %v", err)
	}
	// Valid magic but corrupt payload.
	_, _, _, err = Process(context.Background(), append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 32)...), DefaultOptions(1<<30, 8000))
	if !errors.Is(err, ErrDecodeFailed) && !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("expected decode failure, got: %v", err)
	}
}

func TestProcessRejectsOversizedDimensions(t *testing.T) {
	data := solidJPEG(t, 400, 400)
	opts := DefaultOptions(1<<30, 100) // 400px image vs 100px cap
	_, _, _, err := Process(context.Background(), data, opts)
	if !errors.Is(err, ErrDimensionsExceed) {
		t.Fatalf("expected dimension rejection, got: %v", err)
	}
}

func TestProcessRejectsOversizedFile(t *testing.T) {
	data := solidJPEG(t, 100, 100)
	opts := DefaultOptions(10, 8000) // 10-byte cap
	_, _, _, err := Process(context.Background(), data, opts)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected size rejection, got: %v", err)
	}
}

func TestEXIFOrientationApplied(t *testing.T) {
	// Orientation 6 = rotated 90° CW: a 200x100 source must be stored
	// as 100x200.
	data := jpegWithEXIFOrientation(t, 200, 100, 6)
	md, _, _, err := Process(context.Background(), data, DefaultOptions(1<<30, 8000))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if md.Width != 100 || md.Height != 200 {
		t.Fatalf("orientation 6 not applied: got %dx%d, want 100x200", md.Width, md.Height)
	}

	// Orientation 3 = 180°: dimensions unchanged.
	data = jpegWithEXIFOrientation(t, 200, 100, 3)
	md, _, _, err = Process(context.Background(), data, DefaultOptions(1<<30, 8000))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if md.Width != 200 || md.Height != 100 {
		t.Fatalf("orientation 3 changed dimensions: %dx%d", md.Width, md.Height)
	}

	// Orientation 8 = rotate 90° CCW: stored transposed as well.
	data = jpegWithEXIFOrientation(t, 200, 100, 8)
	md, _, _, err = Process(context.Background(), data, DefaultOptions(1<<30, 8000))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if md.Width != 100 || md.Height != 200 {
		t.Fatalf("orientation 8 not applied: got %dx%d", md.Width, md.Height)
	}
}

func TestDuplicateContentSameHash(t *testing.T) {
	a := solidJPEG(t, 300, 200)
	b := solidJPEG(t, 300, 200)
	if !bytes.Equal(a, b) {
		t.Fatal("test setup: identical images must encode identically")
	}
	md1, _, _, err := Process(context.Background(), a, DefaultOptions(1<<30, 8000))
	if err != nil {
		t.Fatal(err)
	}
	md2, _, _, err := Process(context.Background(), b, DefaultOptions(1<<30, 8000))
	if err != nil {
		t.Fatal(err)
	}
	if md1.Hash != md2.Hash {
		t.Fatal("identical bytes produced different hashes")
	}
}
