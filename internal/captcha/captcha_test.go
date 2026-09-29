package captcha

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

func TestEveryAlphabetCharacterHasAGlyph(t *testing.T) {
	for i := 0; i < len(Alphabet); i++ {
		g, ok := glyphs[Alphabet[i]]
		if !ok {
			t.Fatalf("no glyph for %q", Alphabet[i])
		}
		for _, row := range g {
			if len(row) != 5 {
				t.Fatalf("glyph %q has a row of width %d", Alphabet[i], len(row))
			}
		}
	}
}

func TestCodeUsesOnlyTheAlphabet(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := Code(5)
		if len(c) != 5 {
			t.Fatalf("len %d", len(c))
		}
		for _, r := range c {
			if !strings.ContainsRune(Alphabet, r) {
				t.Fatalf("%q not in alphabet", r)
			}
		}
	}
}

func TestMatchIgnoresCaseAndSpaces(t *testing.T) {
	cases := []struct {
		answer string
		ok     bool
	}{{"AC3K7", true}, {"ac3k7", true}, {" a c3k7 ", true}, {"AC3K", false}, {"", false}}
	for _, c := range cases {
		if Match("AC3K7", c.answer) != c.ok {
			t.Errorf("Match(%q) != %v", c.answer, c.ok)
		}
	}
}

func TestRenderProducesADecodablePNG(t *testing.T) {
	b, err := Render(Code(5))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() < 200 || img.Bounds().Dy() < 60 {
		t.Fatalf("image too small: %v", img.Bounds())
	}
}
