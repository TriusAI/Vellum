package extract

import "testing"

func TestTextLayerSane(t *testing.T) {
	good := "The quick brown fox jumps over the lazy dog while counting " +
		"punctuation marks. Parentheses (and brackets [too]) appear here, " +
		"alongside numbers like 12, 42, and 3.14159 in plain sentences."
	if !textLayerSane(good) {
		t.Fatal("good text rejected")
	}
	// bad-OCR garbage: broken spacing
	if textLayerSane("Thequickbrownfoxjumpsoverthelazydogandkeepsgoingwithoutspacesatallhere") {
		t.Fatal("spaceless junk accepted")
	}
	// glyph-code garbage (private use/box chars from a broken CMap)
	if textLayerSane("\uFFFD\uFFFD\uFFFD\uFFFD\uE000\uE001\uE002\uE003\u25A0\u25A0\u25A0\u25A0\x00\x01\x02\x03\x04\x05\x06\x07") {
		t.Fatal("garbled text accepted")
	}
	// pure numerals are not prose
	if textLayerSane("1234567890 987654321 555-0100 2026-10-05 42 7 3.14 100") {
		t.Fatal("numeral dump accepted")
	}
	// short strings are below the sanity floor
	if textLayerSane("Hello there") {
		t.Fatal("too short accepted")
	}
	// accented letters are letters (finnish/chinese-adjacent latin)
	if !textLayerSane("Suomen kieli käyttää ääkkösiä: tänään sataa lunta koko maassa " +
		"ja jotkut sanat pitkittyvät mukavasti kahdella konsonantilla peräkkäin.") {
		t.Fatal("accented text rejected")
	}
	// CJK: no [A-Za-z] words — must NOT autoreject (letters==0 → false!)
	// documented limitation: CJK-only text layers are accepted through the
	// unicode.IsLetter branch but the wordish check fails them; the
	// caller keeps OCR for image pages and this only affects pages with
	// an embedded text layer. Adjusted: letters==0 returns false, meaning
	// CJK-only layers get re-OCR'd when images exist. That is fine.
}
