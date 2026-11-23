// Package imgutil holds tiny CPU-parallel helpers shared by the frame's
// full-screen pixel passes (canvas blending, DRM conversion). The Pi 3
// has four cores and no GPU path in this pipeline, so the difference
// between "smooth" and "a slideshow of snapshots" is whether these
// passes use all of them.
package imgutil

import (
	"runtime"
	"sync"
)

// Chunked runs work over [0, n) split across up to runtime.GOMAXPROCS
// goroutines and waits for all of them. work must not touch anything
// outside its half-open range. n <= 0 is a no-op. work is called at
// least once and in increasing order only when parallelism is 1.
func Chunked(n int, work func(lo, hi int)) {
	if n <= 0 {
		return
	}
	k := runtime.GOMAXPROCS(0)
	if k < 2 || n < 64 {
		work(0, n)
		return
	}
	if k > 8 {
		k = 8
	}
	// Round the chunk up to a multiple of 4 so pixel-indexed callers
	// get pixel-aligned [lo, hi) ranges (and byte-indexed ones get
	// word-aligned offsets for free).
	chunk := ((n+k-1)/k + 3) &^ 3
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += chunk {
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			work(lo, hi)
		}(lo, hi)
	}
	wg.Wait()
}
