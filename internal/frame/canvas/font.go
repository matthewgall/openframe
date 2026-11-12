package canvas

// Font renders crisp, integer-scaled bitmap text. It pre-renders each
// ASCII glyph once per (font, scale, color) and then blits, so drawing
// a line of text is a handful of blits, not per-pixel font queries.
//
// The base face is the public-domain 7x13 X11 misc-fixed font
// (basicfont.Face7x13 from x/image/font — the stdlib copy of
// image/font/basicfont was removed from the standard library in Go
// 1.26 and x/image carries the same public-domain font). At scale 2 a
// glyph cell is 16x~26 px, which is the "large touch target / readable
// typography" size the spec asks for at 1280x800; scale 3 is used for
// large titles.
//
// Only ASCII is rendered; any other rune draws as a space. The UI
// text is under our control (no user free text in v1), so a full
// Unicode face is not needed.

import (
	"image"
	"image/color"
	"image/draw"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

type Font struct {
	face  font.Face
	scale int

	mu    sync.Mutex
	cache map[cacheKey]*image.NRGBA
}

type cacheKey struct {
	r     rune
	color color.NRGBA
}

// NewFont builds a Font at the given integer scale (>=1).
func NewFont(scale int) *Font {
	if scale < 1 {
		scale = 1
	}
	return &Font{face: basicfont.Face7x13, scale: scale, cache: map[cacheKey]*image.NRGBA{}}
}

// Scale returns the glyph scale factor.
func (f *Font) Scale() int { return f.scale }

// CellW is the advance width in pixels per rune (the 7x13 face has
// a 7px advance; glyph pixels sit in a 6px-wide cell inside it).
func (f *Font) CellW() int { return 7 * f.scale }

// CellH is the height of one glyph cell in pixels.
func (f *Font) CellH() int {
	m := f.face.Metrics()
	return (int(m.Ascent.Ceil()) + int(m.Descent.Ceil())) * f.scale
}

// Width returns the pixel width of s at this font's scale.
func (f *Font) Width(s string) int { return len([]rune(s)) * f.CellW() }

// DrawText blits s starting at (x, y) in col and returns the advance
// width in pixels. y is the top of the glyph cell (not the baseline),
// which keeps layout math simple for the UI.
func (f *Font) DrawText(dst *image.NRGBA, x, y int, s string, col color.NRGBA) int {
	x0 := x
	for _, r := range s {
		if r < 32 || r > 126 {
			r = ' '
		}
		g := f.glyph(r, col)
		b := g.Bounds()
		draw.Draw(dst, dst.Rect.Intersect(image.Rect(x, y, x+b.Dx(), y+b.Dy())), g, image.Point{}, draw.Over)
		x += f.CellW()
	}
	return x - x0
}

// glyph returns the tinted, scaled glyph image for r in col, rendering
// it on first use.
func (f *Font) glyph(r rune, col color.NRGBA) *image.NRGBA {
	k := cacheKey{r, col}
	f.mu.Lock()
	defer f.mu.Unlock()
	if g, ok := f.cache[k]; ok {
		return g
	}

	m := f.face.Metrics()
	ascent := int(m.Ascent.Ceil())
	descent := int(m.Descent.Ceil())
	H1 := ascent + descent // 1x cell height
	w1 := 7                // 1x advance (glyph pixels are 6 wide; column 6 is the gap)
	cellW := w1 * f.scale
	cellH := H1 * f.scale

	// 1. Render the glyph into a 1x alpha mask. The pen (baseline)
	//    sits at y=ascent, so the top of the ascent lands at row 0.
	mask1 := image.NewAlpha(image.Rect(0, 0, w1, H1))
	if dr, glyphMask, maskp, _, ok := f.face.Glyph(fixed.Point26_6{X: fixed.I(0), Y: fixed.I(ascent)}, r); ok {
		// Note: Face.Glyph documents that the returned mask may be
		// reused by the face on the next call; DrawMask copies it into
		// our own image immediately, so that is safe.
		draw.DrawMask(mask1, dr, glyphMask, maskp, image.NewUniform(color.White), image.Point{}, draw.Over)
	}

	// 2. Tint + integer-scale (nearest) so the result is crisp.
	out := image.NewNRGBA(image.Rect(0, 0, cellW, cellH))
	for sy := 0; sy < H1; sy++ {
		for sx := 0; sx < w1; sx++ {
			a := mask1.Pix[sy*w1+sx]
			if a == 0 {
				continue
			}
			c := color.NRGBA{col.R, col.G, col.B, a}
			for dy := 0; dy < f.scale; dy++ {
				for dx := 0; dx < f.scale; dx++ {
					out.SetNRGBA(sx*f.scale+dx, sy*f.scale+dy, c)
				}
			}
		}
	}
	f.cache[k] = out
	return out
}
