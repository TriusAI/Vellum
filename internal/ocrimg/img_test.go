package ocrimg

import (
	"testing"
)

// makeWhite returns a blank page: h rows × w cols of white.
func makeWhite(w, h int) *Image {
	return &Image{W: w, H: h, Pix: bytesOf(byte(255), w*h)}
}

func bytesOf(v byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}

// drawInk paints an axis-aligned ink band (dark) at column range.
func drawInk(im *Image, x0, x1, y0, y1 int) {
	for y := y0; y < y1 && y < im.H; y++ {
		for x := x0; x < x1 && x < im.W; x++ {
			im.Pix[y*im.W+x] = 0
		}
	}
}

func TestDecodePGM(t *testing.T) {
	// P5 binary
	img, err := DecodePGM([]byte("P5\n3 2\n255\n\x01\x02\x03\xfe\xff\x80"))
	if err != nil {
		t.Fatal(err)
	}
	if img.W != 3 || img.H != 2 || img.Pix[0] != 1 || img.Pix[5] != 0x80 {
		t.Fatalf("P5 decode wrong: %+v", img)
	}
	// comments + P2 ascii
	img, err = DecodePGM([]byte("P2 # comment\n# more\n3 2\n255\n1 2 3\n254 255 128\n"))
	if err != nil {
		t.Fatal(err)
	}
	if img.W != 3 || img.H != 2 || img.Pix[5] != 128 {
		t.Fatalf("P2 decode wrong: %+v", img)
	}
	if _, err := DecodePGM([]byte("P6\nwhatever")); err == nil {
		t.Fatal("P6 must be rejected")
	}
}

func TestRotate(t *testing.T) {
	im := &Image{W: 3, H: 2, Pix: []byte{
		1, 2, 3,
		4, 5, 6,
	}}
	r90 := im.Rotate(90)
	if r90.W != 2 || r90.H != 3 {
		t.Fatalf("90 dims: %dx%d", r90.W, r90.H)
	}
	// clockwise: top-left of result = bottom-left of source
	if r90.Pix[0] != 4 || r90.Pix[1] != 1 || r90.Pix[2] != 5 || r90.Pix[3] != 2 {
		t.Fatalf("rot90 wrong: %v", r90.Pix)
	}
	r180 := im.Rotate(180)
	if r180.Pix[0] != 6 || r180.Pix[5] != 1 {
		t.Fatalf("rot180 wrong: %v", r180.Pix)
	}
	r270 := im.Rotate(270)
	if r270.W != 2 || r270.H != 3 {
		t.Fatalf("270 dims: %dx%d", r270.W, r270.H)
	}
	// ccw: top-left = top-right of source
	if r270.Pix[0] != 3 || r270.Pix[1] != 6 || r270.Pix[2] != 2 || r270.Pix[3] != 5 {
		t.Fatalf("rot270 wrong: %v", r270.Pix)
	}
	// 90 then 270 = identity
	back := r90.Rotate(270)
	if !equalImg(back, im) {
		t.Fatal("90+270 != identity")
	}
}

func equalImg(a, b *Image) bool {
	if a.W != b.W || a.H != b.H {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}

func TestDownscale(t *testing.T) {
	im := &Image{W: 4, H: 2, Pix: []byte{
		0, 0, 200, 200,
		0, 0, 200, 200,
	}}
	d := im.Downscale(2)
	if d.W != 2 || d.H != 1 || d.Pix[0] != 0 || d.Pix[1] != 200 {
		t.Fatalf("downscale wrong: %+v", d)
	}
}

func TestDetectSpineAccept(t *testing.T) {
	// fake spread: 1190 wide, left block x 70-560, gutter 560-640 (white),
	// right block 640-1120; text rows from y=100..700
	im := makeWhite(1190, 800)
	for y := 100; y < 700; y += 12 { // line pitch
		drawInk(im, 70, 560, y, y+7)
		drawInk(im, 640, 1120, y, y+7)
	}
	at, ok := DetectSpine(im)
	if !ok {
		t.Fatal("spread not detected")
	}
	if at < 520 || at > 680 {
		t.Fatalf("spine at %d, expected ~600±80", at)
	}
}

func TestDetectSpineRejectSinglePage(t *testing.T) {
	// single text block with normal margins — no gutter
	im := makeWhite(1000, 800)
	for y := 100; y < 700; y += 12 {
		drawInk(im, 80, 900, y, y+7)
	}
	if _, ok := DetectSpine(im); ok {
		t.Fatal("single text block misread as a spread")
	}
	// blank page
	if _, ok := DetectSpine(makeWhite(1000, 800)); ok {
		t.Fatal("blank page misread as a spread")
	}
	// a big central ink block (photo/graphic) — no white valley
	blk := makeWhite(1000, 800)
	drawInk(blk, 100, 900, 100, 700)
	if _, ok := DetectSpine(blk); ok {
		t.Fatal("solid block misread as a spread")
	}
}

func TestSplitTwoUp(t *testing.T) {
	im := makeWhite(1190, 800)
	drawInk(im, 70, 560, 100, 200)   // left ink
	drawInk(im, 640, 1120, 100, 200) // right ink
	l, r := im.SplitTwoUp(590)
	if l.W+r.W > im.W || l.H != im.H || r.H != im.H ||
		l.W < im.W/2-100 || r.W < im.W/2-100 {
		t.Fatalf("split dims: %d+%d (src %d)", l.W, r.W, im.W)
	}
	// left half must not contain right-half ink columns
	lf := l.ColFractions(110)
	for x := 560; x < l.W; x++ {
		if lf[x] > 0 {
			t.Fatalf("left half has ink at x=%d (seam dragged)", x)
		}
	}
	// right ink present in right half
	rf := r.ColFractions(110)
	found := false
	for _, f := range rf {
		if f > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("right half lost its content")
	}
}
