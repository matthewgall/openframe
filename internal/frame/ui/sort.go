package ui

import (
	"sort"
	"strings"
)

// sortPhotosByDate orders the canonical collection: newest capture
// first, unknown dates last, name as the tie-break. This is the
// slideshow's default order (spec §10: "sequential or random
// ordering" — random is a per-cycle shuffle of this list).
func sortPhotosByDate(ps []Photo) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i].CaptureDate, ps[j].CaptureDate
		if a.IsZero() && b.IsZero() {
			return ps[i].Name < ps[j].Name
		}
		if a.IsZero() {
			return false
		}
		if b.IsZero() {
			return true
		}
		if !a.Equal(b) {
			return a.After(b)
		}
		return ps[i].Name < ps[j].Name
	})
}

func sortPhotosByName(ps []Photo) {
	sort.SliceStable(ps, func(i, j int) bool {
		return strings.ToLower(ps[i].Name) < strings.ToLower(ps[j].Name)
	})
}
