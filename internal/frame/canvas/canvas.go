package canvas

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"sync"

	xdraw "golang.org/x/image/draw"
)

// FitMode selects how a photograph is mapped to the screen.
type FitMode string

const (
	// FitContain shows the whole photograph with black bars
	// (letterbox/pillarbox); aspect preserved.
	FitContain FitMode = "contain"
	// FitCover fills the screen, cropping the overflow at the
	// centre; aspect preserved.
	FitCover FitMode = "cover"
	// FitCrop fills the screen exactly, stretching if the source
	// aspect differs (the "fill" mode).
	FitCrop FitMode = "crop"
)

// Canvas is a single fullscreen framebuffer plus drawing primitives.
// All methods are safe for concurrent use, but the UI serialises
// rendering anyway: the canvas is presented to the driver only at the
// end of a UI frame.
type Canvas struct {
	W, H int
	img  *image.NRGBA

	// Font cache per scale.
	fm    sync.Mutex
	fonts map[int]*Font

	// Decoded-photo cache, bounded by clearing on overflow. Photo
	// files are content-addressed and immutable, so path is a safe
	// key and an evicted entry is just re-decoded.
	pm  sync.Mutex
	phs map[string]image.Image
}

// New creates a WxH canvas.
func New(w, h int) *Canvas {
	return &Canvas{
		W:     w,
		H:     h,
		img:   image.NewNRGBA(image.Rect(0, 0, w, h)),
		fonts: map[int]*Font{},
		phs:   map[string]image.Image{},
	}
}

// Image returns the underlying framebuffer (read-only contract: the
// driver copies it on present).
func (c *Canvas) Image() *image.NRGBA { return c.img }

// Font returns the Font at scale s (memoised).
func (c *Canvas) Font(s int) *Font {
	c.fm.Lock()
	defer c.fm.Unlock()
	if f, ok := c.fonts[s]; ok {
		return f
	}
	f := NewFont(s)
	c.fonts[s] = f
	return f
}

// At returns the pixel at (x, y) (tests / debug). Panics out of
// bounds, like the underlying image.
func (c *Canvas) At(x, y int) color.NRGBA {
	if x < 0 || y < 0 || x >= c.W || y >= c.H {
		panic(fmt.Sprintf("canvas: At(%d, %d) out of bounds", x, y))
	}
	return c.img.At(x, y).(color.NRGBA)
}

// Fill repaints the whole canvas with col.
func (c *Canvas) Fill(col color.NRGBA) {
	for y := 0; y < c.H; y++ {
		for x := 0; x < c.W; x++ {
			c.img.SetNRGBA(x, y, col)
		}
	}
}

// FillRect paints a rectangle (clipped to the canvas) with col,
// over-blending when alpha < 255.
func (c *Canvas) FillRect(x, y, w, h int, col color.NRGBA) {
	r := c.clampRect(x, y, w, h)
	if col.A == 255 {
		for yy := r.Min.Y; yy < r.Max.Y; yy++ {
			for xx := r.Min.X; xx < r.Max.X; xx++ {
				c.img.SetNRGBA(xx, yy, col)
			}
		}
		return
	}
	if col.A == 0 {
		return
	}
	// Manual un-premultiplied "over" blend: image/draw assumes
	// premultiplied sources, so a plain uniform colour would blend
	// incorrectly.
	a := int(col.A)
	inv := 255 - a
	for yy := r.Min.Y; yy < r.Max.Y; yy++ {
		for xx := r.Min.X; xx < r.Max.X; xx++ {
			d := c.img.At(xx, yy).(color.NRGBA)
			da := int(d.A)
			outA := a + da*inv/255
			if outA == 0 {
				continue
			}
			outR := (int(col.R)*a + int(d.R)*da*inv/255) / outA
			outG := (int(col.G)*a + int(d.G)*da*inv/255) / outA
			outB := (int(col.B)*a + int(d.B)*da*inv/255) / outA
			c.img.SetNRGBA(xx, yy, color.NRGBA{uint8(outR), uint8(outG), uint8(outB), uint8(outA)})
		}
	}
}

// Polygon fills an arbitrary polygon with col (scanline fill, even-
// odd rule). Used for the UI's star glyph (the bitmap font is ASCII
// only).
func (c *Canvas) Polygon(pts [][2]int, col color.NRGBA) {
	if len(pts) < 3 || col.A == 0 {
		return
	}
	var minY, maxY int
	for _, p := range pts {
		if p[1] < minY {
			minY = p[1]
		}
		if p[1] > maxY {
			maxY = p[1]
		}
	}
	for y := minY; y <= maxY; y++ {
		var xs []int
		for i := 0; i < len(pts); i++ {
			a, b := pts[i], pts[(i+1)%len(pts)]
			if (a[1] <= y && b[1] > y) || (b[1] <= y && a[1] > y) {
				x := a[0] + (y-a[1])*(b[0]-a[0])/(b[1]-a[1])
				xs = append(xs, x)
			}
		}
		for i := 0; i+1 < len(xs); i += 2 {
			if xs[i] > xs[i+1] {
				xs[i], xs[i+1] = xs[i+1], xs[i]
			}
			c.FillRect(xs[i], y, xs[i+1]-xs[i], 1, col)
		}
	}
}

// HLine / VLine convenience primitives for separators.
func (c *Canvas) HLine(x0, x1, y int, col color.NRGBA) {
	c.FillRect(min(x0, x1), y, absi(x1-x0), 1, col)
}
func (c *Canvas) VLine(x, y0, y1 int, col color.NRGBA) {
	c.FillRect(x, min(y0, y1), 1, absi(y1-y0), col)
}

func absi(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func (c *Canvas) clampRect(x, y, w, h int) image.Rectangle {
	r := image.Rect(x, y, x+w, y+h).Intersect(c.img.Rect)
	return r
}

// Load decodes a photograph from path (cached). Decode failures are
// returned; a missing file is an error, which the UI renders as a
// placeholder rather than crashing the renderer (spec §10: clear
// error states).
func (c *Canvas) Load(path string) (image.Image, error) {
	c.pm.Lock()
	defer c.pm.Unlock()
	if img, ok := c.phs[path]; ok {
		return img, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("canvas: open %s: %w", path, err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("canvas: decode %s: %w", path, err)
	}
	if len(c.phs) >= 24 {
		c.phs = map[string]image.Image{}
	}
	c.phs[path] = img
	return img, nil
}

// Fit renders src into a w x h frame according to mode. The result is
// always exactly w x h (contain letterboxes with black).
func (c *Canvas) Fit(src image.Image, w, h int, mode FitMode) *image.NRGBA {
	// Opaque black base: contain letterboxes must present as black,
	// not as transparency that would leak whatever was under the
	// photo.
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i+3 < len(out.Pix); i += 4 {
		out.Pix[i+3] = 255
	}
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return out
	}
	var sr image.Rectangle
	var dr image.Rectangle
	switch mode {
	case FitContain:
		s := math.Min(float64(w)/float64(sw), float64(h)/float64(sh))
		dw, dh := int(float64(sw)*s), int(float64(sh)*s)
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
		dr = image.Rect((w-dw)/2, (h-dh)/2, (w-dw)/2+dw, (h-dh)/2+dh)
		sr = sb
	case FitCover:
		// Scale to cover the screen, then take the centre source
		// rectangle that maps exactly onto the destination.
		s := math.Max(float64(w)/float64(sw), float64(h)/float64(sh))
		swt, sht := int(math.Ceil(float64(w)/s)), int(math.Ceil(float64(h)/s))
		sx0 := sb.Min.X + (sw-swt)/2
		sy0 := sb.Min.Y + (sh-sht)/2
		sr = image.Rect(sx0, sy0, sx0+swt, sy0+sht).Intersect(sb)
		if sr.Empty() {
			sr = sb
		}
		dr = image.Rect(0, 0, w, h)
	default: // FitCrop (fill / stretch)
		sr, dr = sb, image.Rect(0, 0, w, h)
	}
	xdraw.BiLinear.Scale(out, dr, src, sr, xdraw.Src, nil)
	return out
}

// FitPhoto is Load + Fit in one call.
func (c *Canvas) FitPhoto(path string, w, h int, mode FitMode) (*image.NRGBA, error) {
	src, err := c.Load(path)
	if err != nil {
		return nil, err
	}
	return c.Fit(src, w, h, mode), nil
}

// BlendCrossfade blends top over base at opacity t in [0,1] and writes
// into dst (t=0 -> base, t=1 -> top). All three must share a bounds.
func BlendCrossfade(dst, base, top *image.NRGBA, t float64) {
	if t <= 0 {
		draw.Draw(dst, dst.Rect, base, image.Point{}, draw.Src)
		return
	}
	if t >= 1 {
		draw.Draw(dst, dst.Rect, top, image.Point{}, draw.Src)
		return
	}
	db := dst.Pix
	bb := base.Pix
	tb := top.Pix
	n := len(db)
	for i := 0; i < n; i++ {
		db[i] = uint8(float64(bb[i])*(1-t) + float64(tb[i])*t)
	}
}

// SlideFrame composites a horizontal slide: base exits toward
// dir*(-1) (left) and top enters from the opposite side, at progress
// t in [0,1]. All three must share a bounds.
func SlideFrame(dst, base, top *image.NRGBA, t float64) {
	if t <= 0 {
		draw.Draw(dst, dst.Rect, base, image.Point{}, draw.Src)
		return
	}
	if t >= 1 {
		draw.Draw(dst, dst.Rect, top, image.Point{}, draw.Src)
		return
	}
	w := dst.Rect.Dx()
	off := int(float64(w) * t)
	// Black background, then both images offset. In draw.Draw the
	// source point sp maps to dst's origin, so sp=(off,0) shifts the
	// image LEFT and sp=(-off,0) shifts it RIGHT.
	draw.Draw(dst, dst.Rect, image.NewUniform(color.NRGBA{}), image.Point{}, draw.Src)
	// base exits toward the left: it keeps its left-aligned band
	// [0, w-off) and its right side slides off the screen.
	draw.Draw(dst, dst.Rect, base, image.Pt(off, 0), draw.Over)
	// top enters from the right: it occupies the right-aligned band
	// [off, w) and grows from the right edge.
	draw.Draw(dst, dst.Rect, top, image.Pt(-off, 0), draw.Over)
}

// SnapshotPNG writes an atomic PNG of the current framebuffer.
func (c *Canvas) SnapshotPNG(path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = png.Encode(f, c.img)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Scratch returns a new empty NRGBA the size of the canvas (used by
// the UI to stage a photo before transition compositing).
func (c *Canvas) Scratch() *image.NRGBA {
	return image.NewNRGBA(image.Rect(0, 0, c.W, c.H))
}
