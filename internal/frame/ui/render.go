package ui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/frame/canvas"
)

// Palette: high-contrast, accessible (spec §10).
var (
	colBg     = color.NRGBA{0, 0, 0, 255}
	colText   = color.NRGBA{255, 255, 255, 255}
	colDim    = color.NRGBA{170, 170, 170, 255}
	colAccent = color.NRGBA{255, 196, 0, 255} // warm highlight (star, focus)
	colButton = color.NRGBA{255, 255, 255, 46}
)

const (
	margin         = 24
	fontBody       = 2 // scale 2
	fontTitle      = 3 // scale 3
	rowHeight      = 96
	galleryVisible = 6
	searchVisible  = 6
	gridPerRow     = 3
	gridRows       = 3
	letterIndexW   = 48
)

// render composites the current screen into the canvas and presents.
func (e *Engine) render() {
	e.buttons = nil
	e.cv.Fill(colBg)

	switch e.screen {
	case ScreenSlideshow:
		e.renderSlideshow()
	case ScreenDetails:
		e.renderDetails()
	case ScreenGallery:
		e.renderGallery()
	case ScreenAlbum:
		e.renderAlbum()
	case ScreenSearch:
		e.renderSearch()
	case ScreenSettings:
		e.renderSettings()
	case ScreenSleep:
		// Black frame; the driver's power state handles the
		// backlight (spec §11).
	}
	e.present()
}

// fitPhoto decodes + fits a photograph to the full canvas (or nil on
// error — the UI shows a placeholder tile instead, spec §10 "clear
// error states").
func (e *Engine) fitPhoto(p *Photo, w, h int) *image.NRGBA {
	if p == nil || p.DisplayPath == "" {
		return nil
	}
	img, err := e.cv.FitPhoto(p.DisplayPath, w, h, e.cfg.Fit)
	if err != nil {
		e.log.Warn("photo decode failed; showing placeholder",
			"photo", p.Hash[:12], "error", err)
		return nil
	}
	return img
}

func (e *Engine) renderSlideshow() {
	p := e.currentPhotoLocked()
	if p == nil {
		e.renderEmpty()
		return
	}
	if e.curImage == nil {
		e.curImage = e.fitPhoto(p, e.w, e.h)
	}
	if e.curImage != nil {
		blit(e.cv.Image(), e.curImage)
	} else {
		// Placeholder tile for an undecodable file.
		e.cv.Fill(colBg)
		e.cv.FillRect(e.w/2-160, e.h/2-40, 320, 80, color.NRGBA{40, 40, 40, 255})
		e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-140, e.h/2-12, "image unavailable", colDim)
	}

	// Optional clock (top right).
	if e.cfg.ShowClock {
		e.drawClock()
	}

	// Optional caption bar (bottom).
	if e.cfg.ShowCaption && (p.Name != "" || p.Caption != "" || !p.CaptureDate.IsZero()) {
		e.drawCaption(p)
	}

	// Controls overlay (tap to reveal; auto-hides after 5s).
	if e.controls {
		e.drawControls(p)
	}
}

func (e *Engine) drawClock() {
	now := e.cfg.Now()
	s := now.Format("15:04")
	f := e.cv.Font(fontBody)
	w := f.Width(s)
	x := e.w - w - margin*2
	y := margin
	e.cv.FillRect(x-8, y-6, w+16, f.CellH()+12, color.NRGBA{0, 0, 0, 140})
	f.DrawText(e.cv.Image(), x, y, s, colText)
}

func (e *Engine) drawCaption(p *Photo) {
	barH := 96
	y := e.h - barH
	e.cv.FillRect(0, y, e.w, barH, color.NRGBA{0, 0, 0, 160})
	f := e.cv.Font(fontBody)
	line1 := p.Name
	line2 := ""
	if p.Caption != "" {
		line2 = p.Caption
	}
	if !p.CaptureDate.IsZero() {
		d := p.CaptureDate.Format("2 Jan 2006")
		if line2 == "" {
			line2 = d
		} else {
			line2 = d + "  —  " + line2
		}
	}
	if line1 != "" {
		f.DrawText(e.cv.Image(), margin*2, y+14, truncate(line1, 40), colText)
	}
	if line2 != "" {
		e.cv.Font(fontBody).DrawText(e.cv.Image(), margin*2, y+52, truncate(line2, 60), colDim)
	}
	// Favourite indicator (above the bar, top-left).
	if p.Favourite {
		drawStar(e.cv, margin*2+14, y-20, 12, colAccent)
		e.cv.Font(fontBody).DrawText(e.cv.Image(), margin*2+36, y-32, "FAVOURITE", colAccent)
	}
}

func (e *Engine) drawControls(p *Photo) {
	e.cv.FillRect(0, e.h-margin-88, e.w, margin+88, color.NRGBA{0, 0, 0, 200})
	e.cv.FillRect(0, 0, e.w, margin+48, color.NRGBA{0, 0, 0, 160})
	// Photo name in the top bar.
	e.cv.Font(fontBody).DrawText(e.cv.Image(), margin, margin+8, truncate(p.Name, 44), colText)

	// Bottom buttons: [‹ Prev]  [Gallery]  [Info]  [Star/Unstar]  [Next ›]
	type bdef struct {
		label string
		kind  int
	}
	bw, bh, gap := 220, 72, 16
	total := 5*bw + 4*gap
	x := (e.w - total) / 2
	y := e.h - margin - bh
	starLabel := "Star photo"
	if p.Favourite {
		starLabel = "Unstar"
	}
	for _, b := range []bdef{
		{"‹ Prev", btnPrev},
		{"Gallery", btnGallery},
		{"Details", btnInfo},
		{starLabel, btnStar},
		{"Next ›", btnNext},
	} {
		e.cv.FillRect(x, y, bw, bh, colButton)
		f := e.cv.Font(fontBody)
		f.DrawText(e.cv.Image(), x+(bw-f.Width(b.label))/2, y+(bh-f.CellH())/2, b.label, colText)
		e.buttons = append(e.buttons, hitButton{label: b.label, rect: [4]int{x, y, x + bw, y + bh}, kind: b.kind})
		x += bw + gap
	}
}

func (e *Engine) renderEmpty() {
	// Clear loading/error state (spec §10).
	info := e.data.Info(context.Background())
	if !info.Paired {
		// M5 pairing screen: a code is waiting for admin approval.
		// Rendered first — it replaces the empty-state text entirely.
		if e.pairingCode != "" {
			e.renderPairing()
			return
		}
		e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-280, e.h/2, "This frame is not set up yet.", colDim)
		e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-280, e.h/2+40, "Pair it from the admin console (Frames → Pending pairing),", colDim)
		e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-280, e.h/2+80, "or set a credential manually (openframe pair).", colDim)
		return
	}
	f := e.cv.Font(fontTitle)
	msg := "No photos yet"
	e.cv.Font(fontTitle).DrawText(e.cv.Image(), (e.w-f.Width(msg))/2, e.h/2-80, msg, colText)
	e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-200, e.h/2, "Waiting for the first synchronisation…", colDim)
	if info.LastSyncError != "" {
		e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-320, e.h/2+40, "Last sync error: "+truncate(info.LastSyncError, 50), colDim)
	}
}

// renderPairing is the M5 not-yet-paired screen with an active
// challenge (spec §12): a large code the administrator types into the
// management console to approve this frame.
func (e *Engine) renderPairing() {
	codeFont := e.cv.Font(8) // 8×7px per cell: 8-char code ≈ 450px wide
	title := "Pair this frame"
	e.cv.Font(fontTitle).DrawText(e.cv.Image(), (e.w-e.cv.Font(fontTitle).Width(title))/2, e.h/2-150, title, colText)

	e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-340, e.h/2-90, "Enter this code in the OpenFrame admin console:", colDim)

	code := e.pairingCode
	e.cv.Font(8).DrawText(e.cv.Image(), (e.w-codeFont.Width(code))/2, e.h/2-60, code, colText)

	e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-300, e.h/2+70, "The code is single-use and expires in 15 minutes.", colDim)
	e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-260, e.h/2+110, "Photos start arriving as soon as it is approved.", colDim)
}

func (e *Engine) renderDetails() {
	p := e.currentPhotoLocked()
	if p == nil {
		e.goGallery()
		return
	}
	// Photo on the top 68%.
	ph := e.h * 68 / 100
	if bandF := e.fitPhoto(p, e.w, ph); bandF != nil {
		copyBand(e.cv.Image(), bandF, 0, 0, e.w, ph)
	}
	// Panel.
	by := ph
	e.cv.FillRect(0, by, e.w, e.h-by, color.NRGBA{24, 24, 24, 255})
	f := e.cv.Font(fontBody)
	e.cv.Font(fontBody).DrawText(e.cv.Image(), margin, by+16, truncate(p.Name, 46), colText)
	y := by + 56
	if p.Caption != "" {
		e.cv.Font(fontBody).DrawText(e.cv.Image(), margin, y, truncate(p.Caption, 80), colDim)
		y += 40
	}
	meta := ""
	if !p.CaptureDate.IsZero() {
		meta = p.CaptureDate.Format("2 Jan 2006 15:04")
	}
	if p.AlbumName != "" {
		if meta != "" {
			meta += "   ·   "
		}
		meta += "Album: " + p.AlbumName
	}
	if p.Uploader != "" {
		if meta != "" {
			meta += "   ·   "
		}
		meta += "By " + p.Uploader
	}
	if meta != "" {
		e.cv.Font(fontBody).DrawText(e.cv.Image(), margin, y, truncate(meta, 80), colDim)
	}
	_ = f
	// Buttons.
	bw, bh, gap := 200, 72, 14
	x := margin
	yb := e.h - margin - bh
	bdefs := []struct {
		label string
		kind  int
	}{
		{"‹ Prev", btnPrev},
		{"Next ›", btnNext},
		{"Star photo", btnStar},
		{"Back", btnBack},
	}
	if p.Favourite {
		bdefs[2].label = "Unstar"
	}
	for _, b := range bdefs {
		e.cv.FillRect(x, yb, bw, bh, colButton)
		ff := e.cv.Font(fontBody)
		ff.DrawText(e.cv.Image(), x+(bw-ff.Width(b.label))/2, yb+(bh-ff.CellH())/2, b.label, colText)
		e.buttons = append(e.buttons, hitButton{label: b.label, rect: [4]int{x, yb, x + bw, yb + bh}, kind: b.kind})
		x += bw + gap
	}
	// Hint.
	e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w-margin-260, yb+24, "swipe up: back", colDim)
}

func (e *Engine) galleryRows() []struct {
	label string
	kind  int
	arg   string
} {
	rows := []struct {
		label string
		kind  int
		arg   string
	}{
		{"Slideshow", btnSlideshow, ""},
	}
	favN := 0
	for _, p := range e.all {
		if p.Favourite {
			favN++
		}
	}
	rows = append(rows, struct {
		label string
		kind  int
		arg   string
	}{fmt.Sprintf("Favourites  (%d)", favN), btnFavourites, ""})
	for _, a := range e.albums {
		rows = append(rows, struct {
			label string
			kind  int
			arg   string
		}{fmt.Sprintf("  %s  (%d)", truncate(a.Name, 30), a.Count), btnAlbum, a.ID})
	}
	rows = append(rows,
		struct {
			label string
			kind  int
			arg   string
		}{"  Search photos…", btnSearch, ""},
		struct {
			label string
			kind  int
			arg   string
		}{"  Settings", btnSettings, ""},
	)
	return rows
}

func (e *Engine) renderGallery() {
	e.cv.Font(fontTitle).DrawText(e.cv.Image(), margin, margin, "Albums", colText)
	e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w-margin-160, margin+16, "swipe: scroll", colDim)

	rows := e.galleryRows()
	y0 := margin*2 + 48
	for i, r := range rows {
		idx := i - e.galleryTop
		if idx < 0 || idx >= galleryVisible {
			continue
		}
		y := y0 + idx*rowHeight
		e.cv.FillRect(margin, y, e.w-2*margin, rowHeight-10, color.NRGBA{255, 255, 255, 26})
		e.cv.Font(fontBody).DrawText(e.cv.Image(), margin+16, y+(rowHeight-10)/2-14, r.label, colText)
		e.buttons = append(e.buttons, hitButton{label: r.arg, rect: [4]int{margin, y, e.w - margin, y + rowHeight - 10}, kind: r.kind})
	}
}

func (e *Engine) renderAlbum() {
	// Album title.
	name := "Album"
	for _, a := range e.albums {
		if a.ID == e.listKey {
			name = a.Name
			break
		}
	}
	e.cv.Font(fontTitle).DrawText(e.cv.Image(), margin, margin, truncate(name, 30), colText)
	e.cv.Font(fontBody).DrawText(e.cv.Image(), margin, margin+52,
		fmt.Sprintf("%d photos   ·   tap a photo to view   ·   swipe right: back", len(e.list)), colDim)

	cols := gridPerRow
	gap := 12
	x0, y0 := margin, margin*2+60
	cellW := (e.w - 2*margin - (cols-1)*gap) / cols
	cellH := (e.h - y0 - margin - (gridRows-1)*gap) / gridRows
	for i := e.albumTop; i < len(e.list); i++ {
		pos := i - e.albumTop
		row, col := pos/gridPerRow, pos%gridPerRow
		if row >= gridRows {
			break
		}
		p := &e.list[i]
		x := x0 + col*(cellW+gap)
		y := y0 + row*(cellH+gap)
		thumb := e.fitPhoto(p, cellW, cellH)
		if thumb != nil {
			copyBand(e.cv.Image(), thumb, x, y, cellW, cellH)
		} else {
			e.cv.FillRect(x, y, cellW, cellH, color.NRGBA{40, 40, 40, 255})
			e.cv.Font(fontBody).DrawText(e.cv.Image(), x+12, y+12, "no image", colDim)
		}
		// Priority / favourite markers.
		if p.Favourite {
			drawStar(e.cv, x+18, y+18, 10, colAccent)
		}
		if p.Priority {
			e.cv.Font(fontBody).DrawText(e.cv.Image(), x+44, y+10, "PRIORITY", colAccent)
		}
		e.buttons = append(e.buttons, hitButton{label: p.Hash, rect: [4]int{x, y, x + cellW, y + cellH}, kind: browsePhoto})
	}
}

func (e *Engine) renderSearch() {
	title := "Search — by date"
	if e.sortByName {
		title = "Search — by name"
	}
	e.cv.Font(fontTitle).DrawText(e.cv.Image(), margin, margin, title+"  (tap to change)", colText)
	if e.filterChar != 0 {
		e.cv.Font(fontBody).DrawText(e.cv.Image(), e.w/2-80, margin+52,
			fmt.Sprintf("filtering: %c   (tap again to clear)", e.filterChar), colAccent)
	}

	list := e.listForKey("search")
	y0 := margin*2 + 64
	// Tapping the title toggles sort.
	e.buttons = append(e.buttons, hitButton{label: "sort", rect: [4]int{margin, margin, e.w - letterIndexW, margin + 44}, kind: btnSearch})
	// Re-purpose btnSearch as a sort toggle when label=="sort".
	e.buttons[len(e.buttons)-1].kind = btnSort

	for i, p := range list {
		idx := i - e.searchTop
		if idx < 0 || idx >= searchVisible {
			continue
		}
		y := y0 + idx*rowHeight
		e.cv.FillRect(margin, y, e.w-2*margin-letterIndexW, rowHeight-10, color.NRGBA{255, 255, 255, 26})
		label := truncate(p.Name, 44)
		if e.sortByName {
			if p.CaptureDate.IsZero() {
				label = truncate(p.Name, 60)
			} else {
				label = truncate(p.Name, 44) + "  —  " + p.CaptureDate.Format("2006-01-02")
			}
		} else {
			if !p.CaptureDate.IsZero() {
				label = p.CaptureDate.Format("2006-01-02") + "  —  " + truncate(p.Name, 44)
			}
		}
		e.cv.Font(fontBody).DrawText(e.cv.Image(), margin+16, y+(rowHeight-10)/2-14, label, colText)
		e.buttons = append(e.buttons, hitButton{label: p.Hash, rect: [4]int{margin, y, e.w - margin - letterIndexW, y + rowHeight - 10}, kind: browsePhoto})
	}

	// A-Z letter index on the right edge.
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lh := e.h / 26
	for i := 0; i < 26; i++ {
		y := i * lh
		col := colDim
		if rune(letters[i]) == e.filterChar {
			col = colAccent
		}
		e.cv.FillRect(e.w-letterIndexW, y, letterIndexW, lh, color.NRGBA{0, 0, 0, 120})
		f := e.cv.Font(fontBody)
		f.DrawText(e.cv.Image(), e.w-letterIndexW/2-7, y+(lh-f.CellH())/2, string(letters[i]), col)
	}
}

func (e *Engine) renderSettings() {
	e.cv.Font(fontTitle).DrawText(e.cv.Image(), margin, margin, "Settings", colText)
	y := margin*2 + 56
	rowW := e.w - 2*margin
	y = e.settingRow(y, rowW, "Brightness", fmt.Sprintf("%d%%", int(e.staged.Brightness*100)))
	y = e.settingRow(y, rowW, "Slideshow interval", formatDuration(e.staged.SlideshowInterval))
	y = e.settingRow(y, rowW, "Transition", e.staged.Transition)
	y = e.settingRow(y, rowW, "Orientation", fmt.Sprintf("%d°", e.staged.Orientation))
	y = e.settingRow(y, rowW, "Sleep on", minutesToHM(e.staged.SleepOn))
	y = e.settingRow(y, rowW, "Sleep off", minutesToHM(e.staged.SleepOff))
	capV := "off"
	if e.staged.ShowCaption {
		capV = "on"
	}
	y = e.settingRow(y, rowW, "Captions", capV)
	clkV := "off"
	if e.staged.ShowClock {
		clkV = "on"
	}
	y = e.settingRow(y, rowW, "Clock", clkV)

	// Info block (read-only).
	info := e.data.Info(context.Background())
	e.cv.FillRect(margin, y+8, rowW, 8, color.NRGBA{255, 255, 255, 60})
	y += 28
	e.infoRow(y, rowW, "Device", info.DeviceName)
	y += 44
	reach := "unreachable"
	if info.ServerReachable {
		reach = "reachable"
	}
	e.infoRow(y, rowW, "Server", info.ServerURL+" ("+reach+")")
	y += 44
	last := "never"
	if info.LastSync != nil {
		last = info.LastSync.Format("2006-01-02 15:04")
	}
	e.infoRow(y, rowW, "Last sync", last)
	y += 44
	e.infoRow(y, rowW, "Storage free", formatBytes(info.FreeBytes))
	y += 44
	e.infoRow(y, rowW, "Version", info.Version)
	y += 60

	// Apply (hold-protected).
	bw := 320.0
	bh := 72
	bx := (e.w - int(bw)) / 2
	e.cv.FillRect(bx, y, int(bw), bh, color.NRGBA{255, 196, 0, 46})
	f := e.cv.Font(fontBody)
	label := "HOLD to apply"
	f.DrawText(e.cv.Image(), bx+(int(bw)-f.Width(label))/2, y+(bh-f.CellH())/2, label, colAccent)
	e.buttons = append(e.buttons, hitButton{label: label, rect: [4]int{bx, y, bx + int(bw), y + bh}, kind: btnApply})
}

// settingRow draws a tappable setting row and registers its hit area
// (tap cycles/toggles; for brightness, tap left/right half adjusts).
func (e *Engine) settingRow(y, w int, name, value string) int {
	e.cv.FillRect(margin, y, w, rowHeight-16, color.NRGBA{255, 255, 255, 26})
	e.cv.Font(fontBody).DrawText(e.cv.Image(), margin+16, y+(rowHeight-16)/2-14, name, colText)
	vf := e.cv.Font(fontBody)
	vw := vf.Width(value)
	e.cv.Font(fontBody).DrawText(e.cv.Image(), margin+w-16-vw, y+(rowHeight-16)/2-14, value, colAccent)
	e.buttons = append(e.buttons, hitButton{label: name, rect: [4]int{margin, y, margin + w, y + rowHeight - 16}, kind: btnSetting + settingIndex(name)})
	return y + rowHeight
}

// infoRow draws a read-only info line.
func (e *Engine) infoRow(y, w int, name, value string) {
	e.cv.Font(fontBody).DrawText(e.cv.Image(), margin, y, name+":  ", colDim)
	e.cv.Font(fontBody).DrawText(e.cv.Image(), margin+220, y, truncate(value, 60), colDim)
}

// settingIndex maps a setting name to a stable index for hit-testing.
func settingIndex(name string) int {
	switch name {
	case "Brightness":
		return 0
	case "Slideshow interval":
		return 1
	case "Transition":
		return 2
	case "Orientation":
		return 3
	case "Sleep on":
		return 4
	case "Sleep off":
		return 5
	case "Captions":
		return 6
	default:
		return 7
	}
}

// --- helpers ---------------------------------------------------------

// copyBand draws a rectangular region of src into dst at (dx,dy).
func copyBand(dst, src *image.NRGBA, dx, dy, w, h int) {
	for y := 0; y < h; y++ {
		sy, dy2 := y, dy+y
		if sy < 0 || sy >= src.Rect.Dy() || dy2 < 0 || dy2 >= dst.Rect.Dy() {
			continue
		}
		for x := 0; x < w; x++ {
			sx, dx2 := x, dx+x
			if sx < 0 || sx >= src.Rect.Dx() || dx2 < 0 || dx2 >= dst.Rect.Dx() {
				continue
			}
			dst.SetNRGBA(dx2, dy2, src.At(sx, sy).(color.NRGBA))
		}
	}
}

// composeTransition draws the transition frame (crossfade or slide)
// into the canvas.
func (e *Engine) composeTransition(old, new *image.NRGBA, t float64) {
	switch e.cfg.Transition {
	case "slide":
		canvas.SlideFrame(e.cv.Image(), old, new, t)
	default: // crossfade
		canvas.BlendCrossfade(e.cv.Image(), old, new, t)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		m := int(d.Minutes())
		if m < 60 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dh", m/60)
	}
}

func minutesToHM(min int) string {
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func matchesChar(p Photo, ch rune) bool {
	for _, s := range []string{p.Name, p.Caption} {
		if s == "" {
			continue
		}
		if strings.ToLower(string([]rune(s)[0])) == strings.ToLower(string(ch)) {
			return true
		}
	}
	return false
}

// drawStar draws a five-point star centred at (cx, cy) with outer
// radius r.
func drawStar(cv *canvas.Canvas, cx, cy, r int, col color.NRGBA) {
	var pts [][2]int
	for i := 0; i < 10; i++ {
		// Alternating outer/inner vertices, starting at the top.
		rr := float64(r)
		if i%2 == 1 {
			rr *= 0.45
		}
		a := -math.Pi/2 + math.Pi*float64(i)/5
		pts = append(pts, [2]int{cx + int(rr*math.Cos(a)), cy + int(rr*math.Sin(a))})
	}
	cv.Polygon(pts, col)
}
