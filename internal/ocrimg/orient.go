package ocrimg

// Orientation heuristics for scanned pages that come out rotated.
//
// Two signals are combined:
//
//   - line rhythm: a page of horizontal text has an ink profile over
//     ROWS (column fractions over COLS, equivalently) that alternates
//     between ink bands (lines) and white gaps (leading) in a
//     distinctive narrow-band pattern. Rotating a page 90° swaps which
//     axis shows that pattern, so the axis with the better rhythm score
//     tells us whether the page is portrait-ish or needs a quarter
//     turn. Scoring this on both axes is cheap and robust for CJK and
//     Latin alike.
//   - mass asymmetry: within one text line, the ink is not vertically
//     centered (Latin x-height sits on the baseline; CJK glyphs hang).
//     Comparing per-line ink-centroid against the line's own height
//     yields a consistent sign for upright text and the opposite sign
//     when the page is upside down. The absolute convention is
//     calibrated by tests on rendered text.
//
// Tesseract's OSD (--psm 0, osd.traineddata) does the same job from
// glyph shapes and is preferred when it succeeds; these heuristics are
// the fallback (OSD refuses short pages / missing traineddata).

// RhythmScore rates a 1-D ink profile on text-line structure, as the
// strength of its PERIODICITY: rows crossing a text page alternate
// between ink bands (lines) and white gaps at a stable pitch, giving a
// strong autocorrelation peak at the line pitch. Axes crossing lines
// end-on (page needing a quarter turn) are ragged plateaus with no
// pitch: near-zero score. Scale-independent (pitch in raster pixels).
func RhythmScore(prof []float64) float64 {
	mean := 0.0
	for _, v := range prof {
		mean += v
	}
	mean /= float64(len(prof))
	if mean < 0.0005 {
		return 0 // blank
	}
	// content window: trim the white margins (a rectangular window
	// full of white would bias the correlation)
	top := 0.0
	for _, v := range prof {
		if v > top {
			top = v
		}
	}
	floor := 0.15 * top
	a, b := 0, len(prof)-1
	for ; a < len(prof) && prof[a] < floor; a++ {
	}
	for ; b > a && prof[b] < floor; b-- {
	}
	n := b - a + 1
	if n < 24 {
		return 0 // too little content to speak of
	}
	x := make([]float64, n)
	sum := 0.0
	for i := 0; i < n; i++ {
		x[i] = prof[a+i]
		sum += x[i]
	}
	m := sum / float64(n)
	// discard near-uniform windows (flat profile = no alternating bands)
	spread := 0.0
	for i := range x {
		x[i] -= m
		spread += x[i] * x[i]
	}
	variance := spread / float64(n)
	if variance < 1e-9 {
		return 0
	}
	// normalized autocorrelation at plausible line pitches
	// (4..120 px in the current coordinate space covers 300dpi A4
	// shrunk up to 8x down to full-res 200dpi)
	best := 0.0
	maxLag := n / 4
	if maxLag > 120 {
		maxLag = 120
	}
	for lag := 4; lag <= maxLag; lag++ {
		c, cnt := 0.0, 0.0
		for i := 0; i+lag < n; i++ {
			c += x[i] * x[i+lag]
			cnt++
		}
		if cnt == 0 {
			continue
		}
		r := (c / cnt) / variance // in [-1, 1]
		if r > best {
			best = r
		}
	}
	return best
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s[len(s)/2]
}

// MassAsym measures the average vertical ink-centroid offset within
// text bands of the ROW profile: for each band, the ink centroid's
// offset from the band middle, normalized by the band height, over
// bands; median across bands resists single-line noise. Sign (see
// Orientation doc): positive for upright Latin text — the ink of a
// line sits toward the band's END (higher row index).
func MassAsym(im *Image, threshold uint8) float64 {
	prof := im.RowFractions(threshold)
	mean := 0.0
	for _, v := range prof {
		mean += v
	}
	mean /= float64(len(prof))
	thr := 1.6 * mean
	if mean < 0.0005 {
		return 0
	}
	// band scan (same threshold semantics as rhythmScore)
	var sums []float64 // per band: (centroid - mid)/height, mass-weighted
	start := -1
	var acc, accMassW float64
	flush := func(end int) {
		if start < 0 {
			return
		}
		h := end - start
		if h < 3 || accMassW <= 0 {
			start = -1
			acc, accMassW = 0, 0
			return
		}
		mid := float64(start) + float64(h)/2
		sums = append(sums, (acc/accMassW-mid)/float64(h))
		acc, accMassW = 0, 0
		start = -1
	}
	for i, v := range prof {
		if v > thr {
			if start < 0 {
				start = i
			}
			acc += v * float64(i)
			accMassW += v
		} else {
			flush(i)
		}
	}
	flush(len(prof))
	if len(sums) == 0 {
		return 0
	}
	return median(sums)
}

// Orientation returns the rotation (0/90/180/270) that most likely
// makes the page read upright, with a boolean saying whether the
// decision is meaningful (enough ink, clear axis preference, clear
// asymmetry). Calibration (tests/geometry_test.go, Helvetica text at
// 200dpi): upright pages give MassAsym ~ +0.05; upside-down pages the
// exact opposite — hence positive = upright. CJK pages are near
// symmetric and get abstained from (left alone).
func Orientation(im *Image) (deg int, confident bool) {
	small := im
	for small.W*small.H > 1<<20 {
		small = small.Downscale(2)
	}
	const ink = uint8(110)
	rows := RhythmScore(small.RowFractions(ink))
	cols := RhythmScore(small.ColFractions(ink))
	if rows < 0.15 && cols < 0.15 {
		return 0, false // blank, photo, or patternless: don't touch it
	}
	// quarter-turn needed when the COLUMN profile carries the line
	// rhythm (text lines running vertically)
	if cols > rows*1.3 {
		// cw and ccw candidates differ by 180, so their MassAsym values
		// are exact mirror images; the positive one is the upright one
		a := MassAsym(small.Rotate(90), ink)
		switch {
		case a > 0.02:
			return 90, true
		case a < -0.02:
			return 270, true
		}
		return 0, false
	}
	if rows < 0.15 {
		return 0, false
	}
	// horizontal text: 0 vs 180 via mass asymmetry
	a := MassAsym(small, ink)
	switch {
	case a > 0.02:
		return 0, true
	case a < -0.02:
		return 180, true
	}
	return 0, false // near-symmetric: leave the page alone
}
