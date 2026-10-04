// Package ocrimg analyzes page bitmaps before OCR: PGM decode/rotate,
// ink profiles to detect two-up scanned spreads (the binding gutter
// splitting a two-page scan), coarse 90° orientation signals, and the
// helpers to split a spread into two pages.
//
// Everything here works on plain grayscale pixels (Go slices, one pass)
// so the scan fix-ups stay dependency-free and fast; Tesseract OSD is
// used by the caller for the fine-grained 180° check when it is
// available and the text is long enough.
package ocrimg

import (
	"fmt"
	"strconv"
	"strings"
)

// Image is a small grayscale bitmap: 0 = ink, 255 = paper.
type Image struct {
	W, H int
	Pix  []byte // row-major, len = W*H
}

// DecodePGM parses a binary (P5) or ASCII (P2) PGM with maxval <= 255.
func DecodePGM(data []byte) (*Image, error) {
	// split header: magic, dimensions, maxval — comments (#…\n) allowed
	fields := []string{}
	i := 0
	for len(fields) < 4 && i < len(data) {
		// skip whitespace
		for i < len(data) && (data[i] == ' ' || data[i] == '\n' ||
			data[i] == '\r' || data[i] == '\t') {
			i++
		}
		if i < len(data) && data[i] == '#' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			continue
		}
		start := i
		for i < len(data) && data[i] != ' ' && data[i] != '\n' &&
			data[i] != '\r' && data[i] != '\t' && data[i] != '#' {
			i++
		}
		if i > start {
			fields = append(fields, string(data[start:i]))
		}
	}
	if len(fields) < 4 || (fields[0] != "P5" && fields[0] != "P2") {
		return nil, fmt.Errorf("not a P5/P2 PGM (header fields: %v)", fields)
	}
	w, err1 := strconv.Atoi(fields[1])
	h, err2 := strconv.Atoi(fields[2])
	maxv, err3 := strconv.Atoi(fields[3])
	if err1 != nil || err2 != nil || err3 != nil || w <= 0 || h <= 0 || maxv > 255 {
		return nil, fmt.Errorf("bad PGM header %v", fields)
	}
	i++ // single whitespace after maxval
	pix := make([]byte, 0, w*h)
	if fields[0] == "P5" {
		rest := data[i:]
		if len(rest) < w*h {
			return nil, fmt.Errorf("PGM truncated: need %d bytes, got %d", w*h, len(rest))
		}
		pix = append(pix, rest[:w*h]...)
	} else {
		for _, f := range strings.Fields(string(data[i:])) {
			if len(pix) >= w*h {
				break
			}
			v, err := strconv.Atoi(f)
			if err != nil || v < 0 || v > maxv {
				return nil, fmt.Errorf("bad PGM sample %q", f)
			}
			pix = append(pix, byte(v))
		}
		if len(pix) < w*h {
			return nil, fmt.Errorf("PGM ascii truncated: need %d samples, got %d", w*h, len(pix))
		}
	}
	return &Image{W: w, H: h, Pix: pix}, nil
}

// Rotate returns the image rotated by 90° clockwise (90), 180, or 270°
// (90 counter-clockwise).
func (im *Image) Rotate(deg int) *Image {
	switch deg {
	case 90: // clockwise: R(r, c) = S(col=r, row=H-1-c)
		out := make([]byte, len(im.Pix))
		w, h := im.H, im.W
		for dy := 0; dy < im.W; dy++ {
			for dx := 0; dx < im.H; dx++ {
				out[dy*w+dx] = im.Pix[(im.H-1-dx)*im.W+dy]
			}
		}
		return &Image{W: w, H: h, Pix: out}
	case 180:
		out := make([]byte, len(im.Pix))
		for i := range im.Pix {
			out[i] = im.Pix[len(im.Pix)-1-i]
		}
		return &Image{W: im.W, H: im.H, Pix: out}
	case 270: // counter-clockwise: R(r, c) = S(col=W-1-r, row=c)
		out := make([]byte, len(im.Pix))
		w, h := im.H, im.W
		for dy := 0; dy < im.W; dy++ {
			for dx := 0; dx < im.H; dx++ {
				out[dy*w+dx] = im.Pix[dx*im.W+(im.W-1-dy)]
			}
		}
		return &Image{W: w, H: h, Pix: out}
	}
	return im
}

// Downscale box-means the image by integer factor n (n=1 returns a copy).
func (im *Image) Downscale(n int) *Image {
	if n <= 1 {
		cp := *im
		cp.Pix = append([]byte(nil), im.Pix...)
		return &cp
	}
	w, h := im.W/n, im.H/n
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sum := 0
			for dy := 0; dy < n; dy++ {
				for dx := 0; dx < n; dx++ {
					sum += int(im.Pix[(y*n+dy)*im.W+(x*n+dx)])
				}
			}
			out[y*w+x] = byte(sum / (n * n))
		}
	}
	return &Image{W: w, H: h, Pix: out}
}

// ColFractions returns, per column, the fraction of ink pixels
// (darkness < threshold). One pass, O(W*H).
func (im *Image) ColFractions(threshold uint8) []float64 {
	w, h := im.W, im.H
	counts := make([]int, w)
	for y := 0; y < h; y++ {
		row := im.Pix[y*w : y*w+w]
		for x := 0; x < w; x++ {
			if row[x] < threshold {
				counts[x]++
			}
		}
	}
	out := make([]float64, w)
	for x := range counts {
		out[x] = float64(counts[x]) / float64(h)
	}
	return out
}

// RowFractions returns, per row, the fraction of ink pixels.
func (im *Image) RowFractions(threshold uint8) []float64 {
	w, h := im.W, im.H
	out := make([]float64, h)
	for y := 0; y < h; y++ {
		count := 0
		row := im.Pix[y*w : y*w+w]
		for x := 0; x < w; x++ {
			if row[x] < threshold {
				count++
			}
		}
		out[y] = float64(count) / float64(w)
	}
	return out
}

// spineCandidate reports the deepest low-ink valley within the central
// [40%,60%] band (an open-book scan's gutter is by definition the page
// seam — near the center), plus the ink level on each flank measured
// in [30%,40%] and [60%,70%] (75th percentile = local ink level).
func spineCandidate(prof []float64) (at int, depth, flankL, flankR float64, ok bool) {
	w := len(prof)
	lo, hi := w*42/100, w*58/100
	// smooth with a sliding window ~2% of width (a real gutter is a
	// WIDE white band; this also desensitizes single word-gaps)
	win := w / 50
	if win < 6 {
		win = 6
	}
	best, bestAt := 2.0, -1
	for start := lo; start+win <= hi+win; start++ {
		sum := 0.0
		for j := start; j < start+win; j++ {
			if j >= 0 && j < w {
				sum += prof[j]
			}
		}
		m := sum / float64(win)
		if m < best {
			best, bestAt = m, start+win/2
		}
	}
	if bestAt < 0 {
		return 0, 0, 0, 0, false
	}
	// flanks: ink level (75th percentile) of the nearest thirds
	pct75 := func(from, to int) float64 {
		vals := []float64{}
		for j := from; j < to && j < w; j++ {
			vals = append(vals, prof[j])
		}
		if len(vals) == 0 {
			return 0
		}
		// insertion 75th percentile
		for i := 1; i < len(vals); i++ {
			for j := i; j > 0 && vals[j] < vals[j-1]; j-- {
				vals[j], vals[j-1] = vals[j-1], vals[j]
			}
		}
		return vals[len(vals)*3/4]
	}
	flankL = pct75(w*30/100, bestAt-win/2)
	flankR = pct75(bestAt+win/2, w*70/100)
	return bestAt, best, flankL, flankR, true
}

// DetectSpine decides whether the page is a two-page scan. Returns the
// split column (in the same coordinate space as im) when the classic
// spread signature holds: a near-white vertical band around the page
// center with ink-bearing text on both sides.
func DetectSpine(im *Image) (at int, ok bool) {
	// 1 Mpx analysis is plenty; keep the split integral
	small := im
	for small.W*small.H > 1<<20 {
		small = small.Downscale(2)
	}
	prof := small.ColFractions(110)
	// reference ink level of the page (75th pct overall)
	ref := func() float64 {
		vals := append([]float64(nil), prof...)
		for i := 1; i < len(vals); i++ {
			for j := i; j > 0 && vals[j] < vals[j-1]; j-- {
				vals[j], vals[j-1] = vals[j-1], vals[j]
			}
		}
		return vals[len(vals)*3/4]
	}()
	at, depth, flankL, flankR, found := spineCandidate(prof)
	if !found {
		return 0, false
	}
	flank := flankL
	if flankR < flank {
		flank = flankR
	}
	switch {
	// both flanks must carry text-level ink (page-relative)
	case ref < 0.002 || flank < 0.3*ref:
		return 0, false
	// the valley must be nearly white in absolute terms
	case depth > 0.004:
		return 0, false
	// and far below the flanking ink (contrast)
	case depth > 0.10*flank:
		return 0, false
	}
	col := at * im.W / small.W
	return col, true
}

// SplitTwoUp halves the image at the gutter column (with a little
// margin clearance) into left and right pages.
func (im *Image) SplitTwoUp(at int) (left, right *Image) {
	// shift the seam off any glyphs touching the gutter
	lw := at
	if lw > im.W/14 { // clearance: move a little away from the seam
		lw -= im.W / 200
	}
	rx := at + im.W/200
	left = &Image{W: lw, H: im.H, Pix: make([]byte, 0, lw*im.H)}
	for y := 0; y < im.H; y++ {
		left.Pix = append(left.Pix, im.Pix[y*im.W:y*im.W+lw]...)
	}
	rw := im.W - rx
	right = &Image{W: rw, H: im.H, Pix: make([]byte, 0, rw*im.H)}
	for y := 0; y < im.H; y++ {
		right.Pix = append(right.Pix, im.Pix[y*im.W+rx:(y+1)*im.W]...)
	}
	return left, right
}
