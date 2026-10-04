package extract

import "testing"

func TestOcrQuality(t *testing.T) {
	good := "Chapter Fifteen\n\nunder gathered expected brown over while marks clouds " +
		"earlier the fox lazy counting quietly that punctuation, dogs " +
		"punctuation: under; gathered. The quick (brown) fox jumps!"
	if q := ocrQuality(good); q < 0.55 {
		t.Fatalf("good prose scored %v, want >= 0.55", q)
	}
	flipped := "poseuyeb sapun uoljenjyound shop sduin{ yoinb ueu} yeu} Ajjainb Huljunoo Aze] Xo} au} Jaljyea"
	if q := ocrQuality(flipped); q > 0.35 {
		t.Fatalf("flipped-glyph garbage scored %v, want <= 0.35", q)
	}
	if q := ocrQuality(""); q != 0 {
		t.Fatalf("empty scored %v", q)
	}
	// numbers with normal context still score reasonably (relative use)
	num := "Table 12 shows the results: 42 percent, 3.14159 and 7 of 10 cases studied in 2026."
	if q := ocrQuality(num); q < 0.4 {
		t.Fatalf("numeric prose scored %v, want >= 0.4", q)
	}
}
