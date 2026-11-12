package ui

import (
	"context"
	"image"
	"image/color"
	"time"

	"github.com/matthewgall/openframe/internal/frame/touch"
)

// HandleGesture dispatches a gesture on the current screen. Every
// gesture counts as activity (inactivity timers reset here, not in
// Run, so direct callers behave identically).
func (e *Engine) HandleGesture(g touch.Gesture) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handleGestureLocked(g)
}

// handleGestureLocked is the gesture dispatch; caller holds e.mu.
// Recursion from sub-gestures must go through this, not HandleGesture
// (the lock is not reentrant).
func (e *Engine) handleGestureLocked(g touch.Gesture) {
	e.lastActivity = g.T
	switch g.Kind {
	case touch.KindSwipe:
		e.onSwipe(g.Dir)
	case touch.KindTap:
		e.onTap(g.Pos)
	case touch.KindDoubleTap:
		e.onDoubleTap()
	case touch.KindHold:
		e.onHold(g.Pos)
	}
}

func (e *Engine) onSwipe(dir touch.Dir) {
	switch e.screen {
	case ScreenSlideshow:
		switch dir {
		case touch.DirLeft:
			e.advance(1)
		case touch.DirRight:
			e.advance(-1)
		case touch.DirDown:
			e.goGallery()
		}
	case ScreenDetails:
		switch dir {
		case touch.DirLeft:
			e.advance(1)
		case touch.DirRight:
			e.advance(-1)
		case touch.DirUp:
			e.goSlideshow(e.listKey, e.idx)
		}
	case ScreenGallery:
		switch dir {
		case touch.DirUp:
			e.galleryTop -= 3
		case touch.DirDown:
			e.galleryTop += 3
		case touch.DirRight:
			e.goSlideshow("all", -1)
		}
		e.clampGallery()
		e.render()
	case ScreenAlbum:
		switch dir {
		case touch.DirUp:
			e.albumTop -= gridPerRow * (gridRows - 1)
		case touch.DirDown:
			e.albumTop += gridPerRow * (gridRows - 1)
		case touch.DirRight:
			e.goGallery()
		}
		e.clampAlbum()
		e.render()
	case ScreenSearch:
		switch dir {
		case touch.DirUp:
			e.searchTop -= 4
		case touch.DirDown:
			e.searchTop += 4
		case touch.DirRight:
			e.goGallery()
		}
		e.clampSearch()
		e.render()
	case ScreenSettings:
		if dir == touch.DirRight {
			e.goGallery()
			e.render()
		}
	}
}

func (e *Engine) onTap(pos [2]int) {
	x, y := pos[0], pos[1]
	// Buttons first.
	for i := range e.buttons {
		b := &e.buttons[i]
		if x >= b.rect[0] && x < b.rect[2] && y >= b.rect[1] && y < b.rect[3] {
			e.doButton(b.kind, *b)
			return
		}
	}
	// Screen-specific tap fallbacks.
	switch e.screen {
	case ScreenSlideshow:
		e.controls = true
		e.render()
	case ScreenSearch:
		e.onSearchBackgroundTap(x, y)
	}
}

func (e *Engine) onDoubleTap() {
	// Double-tap on the slideshow opens photo details (the "tap to
	// reveal controls" tap is the single tap; details is one level
	// deeper, per spec §10 "tap to reveal controls" + gallery
	// "fullscreen photograph viewer").
	if e.screen == ScreenSlideshow && e.currentPhotoLocked() != nil {
		e.screen = ScreenDetails
		e.render()
	}
}

func (e *Engine) onHold(pos [2]int) {
	// Hold is the confirm gesture (spec §10: "administrative actions
	// must be protected against accidental activation").
	for i := range e.buttons {
		b := &e.buttons[i]
		if b.kind != btnApply {
			continue
		}
		if pos[0] >= b.rect[0] && pos[0] < b.rect[2] && pos[1] >= b.rect[1] && pos[1] < b.rect[3] {
			e.applySettings()
			return
		}
	}
}

func (e *Engine) doButton(kind int, b hitButton) {
	ctx := context.Background()
	if kind >= btnSetting && kind < btnSetting+8 {
		e.doSetting(kind - btnSetting)
		return
	}
	switch kind {
	case btnPrev:
		e.advance(-1)
	case btnNext:
		e.advance(1)
	case btnStar:
		if p := e.currentPhotoLocked(); p != nil {
			if err := e.data.SetFavourite(ctx, p.Hash, !p.Favourite); err != nil {
				e.log.Warn("set favourite failed", "photo", p.Hash[:12], "error", err)
				return
			}
			for i := range e.all {
				if e.all[i].Hash == p.Hash {
					e.all[i].Favourite = !p.Favourite
				}
			}
			// If we are in the favourites list and just un-favourited,
			// move on (the photo leaves the list).
			if e.listKey == "favourites" && p.Favourite && e.idx >= len(e.listForKey("favourites")) {
				e.list = e.listForKey("favourites")
				if e.idx >= len(e.list) && len(e.list) > 0 {
					e.idx = len(e.list) - 1
				}
			}
			e.render()
		}
	case btnInfo:
		if e.currentPhotoLocked() != nil {
			e.screen = ScreenDetails
			e.render()
		}
	case btnGallery:
		e.goGallery()
	case btnSlideshow:
		e.goSlideshow("all", -1)
	case btnFavourites:
		e.goSlideshow("favourites", 0)
	case btnAlbum:
		e.goAlbum(b.label)
	case btnSearch:
		e.screen = ScreenSearch
		e.searchTop = 0
		e.render()
	case btnSettings:
		e.staged = e.effectiveSettings()
		e.screen = ScreenSettings
		e.render()
	case btnBack:
		switch e.screen {
		case ScreenDetails:
			e.goSlideshow(e.listKey, e.idx)
		default:
			e.goGallery()
		}
	case btnApply:
		// A plain tap on "apply" does nothing by design: the
		// protection is the hold.
		e.log.Info("apply requires a hold")
	case btnSort:
		e.sortByName = !e.sortByName
		e.searchTop = 0
		e.render()
	case browsePhoto:
		e.openPhotoByHash(b.label)
	}
}

// onSearchBackgroundTap handles the A-Z letter index on the right edge.
func (e *Engine) onSearchBackgroundTap(x, y int) {
	if x < e.w-letterIndexW {
		return
	}
	// 26 letters over the full height.
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	idx := (y * 26) / e.h
	if idx < 0 || idx >= 26 {
		return
	}
	ch := rune(letters[idx])
	if e.filterChar == ch {
		e.filterChar = 0 // tap the active letter to clear
	} else {
		e.filterChar = ch
	}
	e.searchTop = 0
	e.render()
}

// advance moves the slideshow ±n positions (wrapping), running the
// configured transition between the old and new photograph (spec §10
// "smooth transitions").
func (e *Engine) advance(n int) {
	if len(e.list) == 0 {
		return
	}
	prevImage := e.curImage
	e.idx = (e.idx + n + len(e.list)*16) % len(e.list)
	e.curImage = e.fitPhoto(&e.list[e.idx], e.w, e.h)
	e.render()
	// Animate the photograph swap under the (already re-drawn)
	// chrome; the final present restores the chrome.
	if prevImage != nil && e.curImage != nil &&
		e.cfg.Transition != "none" && e.cfg.TransitionDuration > 0 {
		e.startTransition(prevImage, e.curImage)
		e.render()
	}
}

// goSlideshow switches to the slideshow on the given list.
func (e *Engine) goSlideshow(listKey string, idx int) {
	e.listKey = listKey
	e.list = e.listForKey(listKey)
	if idx < 0 {
		e.idx = 0
	} else if idx >= len(e.list) {
		e.idx = 0
	} else {
		e.idx = idx
	}
	e.curImage = nil
	e.screen = ScreenSlideshow
	e.controls = false
	e.render()
}

func (e *Engine) goGallery() {
	e.galleryTop = 0
	e.screen = ScreenGallery
	e.render()
}

func (e *Engine) goAlbum(albumID string) {
	e.listKey = albumID
	e.list = e.listForKey(albumID)
	e.idx = 0
	e.albumTop = 0
	e.screen = ScreenAlbum
	e.render()
}

// openPhotoByHash starts the slideshow on the containing list at that
// photo (search row / grid cell taps).
func (e *Engine) openPhotoByHash(hash string) {
	for i, p := range e.all {
		if p.Hash == hash {
			// Use the most specific list that contains the photo.
			if e.screen == ScreenAlbum && p.AlbumID != "" {
				e.goSlideshow(p.AlbumID, i-inAlbumBefore(e.list, p.AlbumID, hash))
			} else {
				e.goSlideshow("all", indexOfHash(e.all, hash))
			}
			return
		}
	}
}

func indexOfHash(photos []Photo, hash string) int {
	for i, p := range photos {
		if p.Hash == hash {
			return i
		}
	}
	return 0
}

func inAlbumBefore(list []Photo, albumID, hash string) int {
	n := 0
	for _, p := range list {
		if p.Hash == hash {
			break
		}
		if p.AlbumID == albumID {
			n++
		}
	}
	return n
}

func (e *Engine) clampGallery() {
	max := len(e.galleryRows()) - galleryVisible
	if max < 0 {
		max = 0
	}
	if e.galleryTop < 0 {
		e.galleryTop = 0
	}
	if e.galleryTop > max {
		e.galleryTop = max
	}
}

func (e *Engine) clampAlbum() {
	if len(e.list) == 0 {
		e.albumTop = 0
		return
	}
	max := (len(e.list) - 1) / (gridPerRow * gridRows) * (gridPerRow * (gridRows - 1))
	if e.albumTop < 0 {
		e.albumTop = 0
	}
	if e.albumTop > max {
		e.albumTop = max
	}
}

func (e *Engine) clampSearch() {
	max := len(e.listForKey("search")) - searchVisible
	if max < 0 {
		max = 0
	}
	if e.searchTop < 0 {
		e.searchTop = 0
	}
	if e.searchTop > max {
		e.searchTop = max
	}
}

// startTransition runs the configured transition between old and new
// photo frames, honouring input (a new gesture finishes the transition
// immediately). Called with the new screen state already applied.
func (e *Engine) startTransition(old, new *image.NRGBA) {
	mode := e.cfg.Transition
	if mode == "none" || e.cfg.TransitionDuration <= 0 || old == nil || new == nil {
		e.presentFull(new)
		return
	}
	steps := int(e.cfg.TransitionDuration / (1000 / 30 * time.Millisecond))
	if steps < 1 {
		steps = 1
	}
	step := time.NewTicker(1000 / 30 * time.Millisecond)
	defer step.Stop()
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		e.composeTransition(old, new, t)
		e.present()
		select {
		case g := <-e.in:
			e.lastActivity = g.T
			e.presentFull(new)
			e.handleGestureLocked(g)
			return
		case <-step.C:
		}
	}
	e.presentFull(new)
}

func (e *Engine) presentFull(img *image.NRGBA) {
	if img == nil {
		return
	}
	blit(e.cv.Image(), img)
	e.present()
}

// blit copies src onto the canvas (same bounds).
func blit(dst, src *image.NRGBA) {
	if dst.Bounds() == src.Bounds() {
		copy(dst.Pix, src.Pix)
	}
}

// darkRect draws a semi-transparent black band (chrome).
func darkRect(e *Engine, x, y, w, h int, alpha uint8) {
	e.cv.FillRect(x, y, w, h, color.NRGBA{0, 0, 0, alpha})
}
