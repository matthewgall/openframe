// Package photos implements the server-side photograph pipeline
// (spec §5: validation, EXIF orientation, metadata, derivatives) and
// the photo domain store.
//
// Security notes (threat model T-04/T-13):
//   - Format is determined by magic bytes, never by file name or the
//     client-supplied Content-Type.
//   - Decoding is dimension-capped before the full decode; oversized or
//     malformed files are rejected.
//   - Derivatives are re-encoded from decoded pixels, so EXIF
//     (including GPS) is structurally stripped from everything a frame
//     ever receives.
package photos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/server/storage"
	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/image/webp"
)

// Validation/size errors mapped to HTTP statuses by the API layer.
var (
	ErrUnsupportedFormat = errors.New("photos: unsupported image format (jpeg, png, webp accepted; heic not in this release)")
	ErrTooLarge          = errors.New("photos: file exceeds upload size limit")
	ErrTooManyFiles      = errors.New("photos: too many files in one upload")
	ErrDecodeFailed      = errors.New("photos: file is not a valid image")
	ErrDimensionsExceed  = errors.New("photos: image exceeds maximum decode dimensions")
	// ErrWebPNotLossy: the pure-Go WebP decoder handles lossy (VP8) only;
	// lossless (VP8L) and extended (VP8X) WebP are rejected with 415.
	ErrWebPNotLossy = errors.New("photos: only lossy WebP is supported in this release")
)

// MIME types accepted in v1 (ADR-0009).
const (
	MIMEJPEG = "image/jpeg"
	MIMEPNG  = "image/png"
	MIMEWebP = "image/webp"
)

// Options configures the pipeline.
type Options struct {
	MaxUploadBytes     int64 // per-file cap
	MaxDecodeDimension int   // px, either axis
	DisplayMaxEdge     int   // long edge of the display derivative
	ThumbSize          int   // square thumbnail edge
}

// DefaultOptions matches the spec's display target (1280x800 panel)
// with headroom for cover/crop layouts.
func DefaultOptions(maxUpload int64, maxDim int) Options {
	return Options{
		MaxUploadBytes:     maxUpload,
		MaxDecodeDimension: maxDim,
		DisplayMaxEdge:     1920,
		ThumbSize:          384,
	}
}

// Metadata is the extracted/derived record for one processed photo.
type Metadata struct {
	Hash        string // sha256 hex of the original bytes
	MIME        string
	Width       int // post-orientation stored dimensions
	Height      int
	SizeBytes   int64
	CaptureDate *time.Time // from EXIF when present
}

// Process validates and decodes an uploaded image, applies EXIF
// orientation, computes the content hash and generates every
// derivative. It returns the original bytes (for storage) and the
// derivative encodings.
//
// The input is fully buffered by the caller (bounded by MaxUploadBytes
// before Process is reached).
func Process(ctx context.Context, data []byte, opts Options) (md Metadata, original []byte, derivatives map[string][]byte, err error) {
	if int64(len(data)) > opts.MaxUploadBytes {
		return Metadata{}, nil, nil, ErrTooLarge
	}
	if len(data) == 0 {
		return Metadata{}, nil, nil, ErrDecodeFailed
	}

	mime, err := sniffFormat(data)
	if err != nil {
		return Metadata{}, nil, nil, err
	}

	// 1. Cheap config decode: dimensions before spending memory.
	cfg, err := decodeConfig(mime, data)
	if err != nil {
		return Metadata{}, nil, nil, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		return Metadata{}, nil, nil, ErrDecodeFailed
	}
	if cfg.Width > opts.MaxDecodeDimension || cfg.Height > opts.MaxDecodeDimension {
		return Metadata{}, nil, nil, fmt.Errorf("%w: %dx%d > %d", ErrDimensionsExceed, cfg.Width, cfg.Height, opts.MaxDecodeDimension)
	}

	// 2. Full decode (bounded by the dimension cap above).
	src, err := decodeImage(mime, data)
	if err != nil {
		return Metadata{}, nil, nil, fmt.Errorf("%w: %v", ErrDecodeFailed, err)
	}

	// 3. EXIF metadata (orientation + capture date). JPEG only: PNG
	//    carries no EXIF in the tags we trust, WebP EXIF is out of v1
	//    scope (documented).
	orientation, captureDate := readEXIF(mime, data)
	img := applyOrientation(src, orientation)

	// 4. Content hash of the ORIGINAL bytes.
	sum := sha256.Sum256(data)
	md = Metadata{
		Hash:        hex.EncodeToString(sum[:]),
		MIME:        mime,
		Width:       img.Bounds().Dx(),
		Height:      img.Bounds().Dy(),
		SizeBytes:   int64(len(data)),
		CaptureDate: captureDate,
	}

	if ctx.Err() != nil {
		return Metadata{}, nil, nil, ctx.Err()
	}

	// 5. Derivatives (re-encoded => EXIF-free by construction).
	derivs, err := generateDerivatives(ctx, img, opts)
	if err != nil {
		return Metadata{}, nil, nil, err
	}
	return md, data, derivs, nil
}

// sniffFormat identifies the image format from magic bytes.
func sniffFormat(data []byte) (string, error) {
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return MIMEJPEG, nil
	}
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return MIMEPNG, nil
	}
	if len(data) >= 16 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		switch string(data[12:16]) {
		case "VP8 ":
			return MIMEWebP, nil // lossy (pure-Go decodable)
		case "VP8L":
			return "", fmt.Errorf("%w: lossless WebP", ErrWebPNotLossy)
		case "VP8X":
			return "", fmt.Errorf("%w: extended WebP", ErrWebPNotLossy)
		default:
			return "", ErrUnsupportedFormat
		}
	}
	// Clear rejection for the most common v2 format (ADR-0009).
	if len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) &&
		(bytes.Equal(data[8:12], []byte("heic")) || bytes.Equal(data[8:12], []byte("heix")) ||
			bytes.Equal(data[8:12], []byte("mif1")) || bytes.Equal(data[8:12], []byte("msf1"))) {
		return "", ErrUnsupportedFormat
	}
	return "", ErrUnsupportedFormat
}

func decodeConfig(mime string, data []byte) (image.Config, error) {
	switch mime {
	case MIMEJPEG:
		return jpeg.DecodeConfig(bytes.NewReader(data))
	case MIMEPNG:
		return png.DecodeConfig(bytes.NewReader(data))
	case MIMEWebP:
		return webp.DecodeConfig(bytes.NewReader(data))
	}
	return image.Config{}, errors.New("unknown mime")
}

func decodeImage(mime string, data []byte) (image.Image, error) {
	switch mime {
	case MIMEJPEG:
		return jpeg.Decode(bytes.NewReader(data))
	case MIMEPNG:
		return png.Decode(bytes.NewReader(data))
	case MIMEWebP:
		return webp.Decode(bytes.NewReader(data))
	}
	return nil, errors.New("unknown mime")
}

// readEXIF extracts orientation (1-8, 0 = none) and capture date.
// Returns (0, nil) when the format carries no EXIF or it is unreadable
// (orientation 0 = leave as-is).
func readEXIF(mime string, data []byte) (int, *time.Time) {
	if mime != MIMEJPEG {
		return 0, nil
	}
	e, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, nil
	}
	orientation := 0
	if tag, err := e.Get(exif.Orientation); err == nil {
		if v, err := tag.Int(0); err == nil {
			orientation = v
		}
	}
	var date *time.Time
	for _, field := range []exif.FieldName{exif.DateTimeOriginal, exif.DateTimeDigitized, exif.DateTime} {
		if tag, err := e.Get(field); err == nil {
			if s, err := tag.StringVal(); err == nil {
				if t, perr := parseEXIFTime(s); perr == nil {
					date = &t
					break
				}
			}
		}
	}
	return orientation, date
}

// parseEXIFTime parses the canonical EXIF timestamp "YYYY:MM:DD HH:MM:SS".
func parseEXIFTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006:01:02 15:04:05", s); err == nil {
		return t, nil
	}
	// Some producers use "-" separators.
	return time.Parse("2006-01-02 15:04:05", s)
}

// generateDerivatives renders every derivative variant to JPEG.
func generateDerivatives(ctx context.Context, img image.Image, opts Options) (map[string][]byte, error) {
	src := toNRGBA(img)

	display := scaleKeepAspect(src, opts.DisplayMaxEdge)
	thumb := centerCropScale(src, opts.ThumbSize, opts.ThumbSize)
	portrait := centerCropScale(src, 1080, 1620)  // 2:3 portrait
	landscape := centerCropScale(src, 1920, 1200) // 16:10 landscape

	out := make(map[string][]byte, 4)
	for variant, im := range map[string]*image.NRGBA{
		storage.VariantDisplay:   display,
		storage.VariantThumb:     thumb,
		storage.VariantPortrait:  portrait,
		storage.VariantLandscape: landscape,
	} {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: 82}); err != nil {
			return nil, fmt.Errorf("encode %s: %w", variant, err)
		}
		out[variant] = buf.Bytes()
	}
	return out, nil
}

// scaleKeepAspect scales so the long edge is maxEdge (no upscaling).
func scaleKeepAspect(src *image.NRGBA, maxEdge int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := float64(maxEdge) / float64(max(w, h))
	if scale >= 1 {
		cp := copyNRGBA(src)
		return cp
	}
	nw := int(float64(w) * scale)
	nh := int(float64(h) * scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	drawBilinear(dst, src, nw, nh)
	return dst
}

// centerCropScale crops the center of src to the target aspect ratio,
// then scales to exactly tw x th.
func centerCropScale(src *image.NRGBA, tw, th int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	targetAR := float64(tw) / float64(th)
	srcAR := float64(w) / float64(h)

	cw, ch := w, h
	if srcAR > targetAR {
		cw = int(float64(h) * targetAR)
	} else {
		ch = int(float64(w) / targetAR)
	}
	if cw < 1 {
		cw = 1
	}
	if ch < 1 {
		ch = 1
	}
	x0 := b.Min.X + (w-cw)/2
	y0 := b.Min.Y + (h-ch)/2
	cropped := copyRect(src, image.Rect(x0, y0, x0+cw, y0+ch))

	dst := image.NewNRGBA(image.Rect(0, 0, tw, th))
	drawBilinear(dst, cropped, tw, th)
	return dst
}
