package extract

// OCR output quality scoring — used to arbitrate between orientation
// candidates (0° vs 180°, 90° vs 270°) and to reject garbage passes.
//
// Why not glyph-shape heuristics: the ink-centroid "bottom-heaviness"
// of Latin lines turned out to flip sign between fixtures (lowercase-
// heavy text vs normal prose, different DPIs), so it can only ORDER
// candidates, never decide them. Why not tesseract confidences: the
// pinned build ships no TSV configs. Why THIS works: upside-down text
// OCR'd directly produces pseudo-words ("poseuyeb") with real-word
// letter statistics but essentially ZERO hits in a common-word
// dictionary, while any real page of English prose is dominated by
// function words. Non-English pages score low in BOTH orientations —
// the comparison then falls back to candidate order (the cheap hint),
// which is the no-fix default.

import (
	"regexp"
	"strings"
)

// commonWords covers the most frequent English function/running words
// plus typical document furniture; enough to separate real prose from
// rotated-glyph pseudo-words decisively.
var commonWords = map[string]bool{}

func init() {
	list := strings.Fields(`a about above across after again against all almost along
already also although always am among an and another any anyone anything are around as
at back be because become been before behind being below between both but by came can
cannot come could did do does doing done down during each early either else enough even
ever every everyone everything few first for found from further get give given go goes
going gone good got had has have having he her here hers herself him himself his how
however i if in indeed instead into is it its itself just keep kept know known last
late later least left less let like likely little long look made make many may maybe me
mean might mind mine more most much must my myself near need never new next no nobody
none nor not nothing now of off often on once one only onto or other others our ours
ourselves out over own page pages part particular per perhaps place put rather really
remain result said same say says see seem seen several shall she should show shown side
since so some someone something still such take taken than that the their them then
there these they thing things think this those though thought three through thus time
to together too took top toward two under until up upon us use used using very via was
way we well went were what when where which while who whole whom whose why will with
within without work would year years yet you your yours yourself
chapter copyright introduction preface foreword published publisher university press
edition volume printed printed translation revised appendix index contents table
figure figures example examples note notes section sections part parts
data model models paper papers study studies research result results
computer computers system systems network networks software machine learning language
processing information theory practice application applications analysis
brown fox lazy dogs jumps counting quietly marks clouds gathered expected
punctuation black white red green blue yellow sun moon star water fire earth
air life death love fear hope dream night day morning evening book books
people man men woman women child children time times day days said says
walk talk read write study learn teach find build open close grow rise fall
change begin start end stop small large big little old young long short
high low light dark warm cold fast slow first second last next near far
left right side sides above below inside outside front back top bottom
hand hands eye eyes head word words line lines point points question
answer questions answers example case cases way ways thing things part`)
	for _, w := range list {
		commonWords[w] = true
	}
}

var (
	reQToken = regexp.MustCompile(`^[A-Za-zÀ-ž0-9][A-Za-zÀ-ž0-9'’\-]*$`)
	reQVowel = regexp.MustCompile(`[aeiouyäöåAEIOUYÄÖÅ]`)
)

// ocrQuality scores OCR output on 0..1: how word-like the tokens are.
// Dictionary hits dominate; general token shape (letters, sane vowel
// ratio, sane length) adds a little.
func ocrQuality(text string) float64 {
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return 0
	}
	dict, shaped := 0, 0
	for _, tok := range tokens {
		tok = strings.Trim(tok, ".,;:!?\"'()[]{}*—–-")
		if tok == "" {
			continue
		}
		if commonWords[strings.ToLower(tok)] {
			dict++
			shaped++
			continue
		}
		if len(tok) >= 2 && len(tok) <= 14 && reQToken.MatchString(tok) {
			v := len(reQVowel.FindAllString(tok, -1))
			if r := float64(v) / float64(len(tok)); r >= 0.15 && r <= 0.70 {
				shaped++
			}
		}
	}
	dictRatio := float64(dict) / float64(len(tokens))
	shapeRatio := float64(shaped) / float64(len(tokens))
	return 0.7*dictRatio + 0.3*shapeRatio
}
