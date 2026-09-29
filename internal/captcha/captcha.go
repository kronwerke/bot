// Package captcha draws a short code as a noisy PNG. It keeps scripts that join
// servers in bulk out; it is not meant to stop a determined person.
package captcha

import (
	"bytes"
	"crypto/rand"
	"image"
	"image/color"
	"image/png"
	"math/big"
	mrand "math/rand/v2"
	"strings"
)

// Alphabet leaves out characters that look alike (0/O, 1/I/L, 2/Z, 5/S, 8/B).
const Alphabet = "ACDEFHJKMNPRTUVWXY34679"

// Code returns n random characters from Alphabet.
func Code(n int) string {
	var b strings.Builder
	max := big.NewInt(int64(len(Alphabet)))
	for i := 0; i < n; i++ {
		k, _ := rand.Int(rand.Reader, max)
		b.WriteByte(Alphabet[k.Int64()])
	}
	return b.String()
}

// Match compares an answer with the code, ignoring case and spaces.
func Match(code, answer string) bool {
	a := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(answer), " ", ""))
	return a != "" && a == code
}

// 5x7 glyphs, one string per row, '#' is ink.
var glyphs = map[byte][7]string{
	'A': {".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'C': {".####", "#....", "#....", "#....", "#....", "#....", ".####"},
	'D': {"####.", "#...#", "#...#", "#...#", "#...#", "#...#", "####."},
	'E': {"#####", "#....", "#....", "####.", "#....", "#....", "#####"},
	'F': {"#####", "#....", "#....", "####.", "#....", "#....", "#...."},
	'H': {"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'J': {"..###", "...#.", "...#.", "...#.", "#..#.", "#..#.", ".##.."},
	'K': {"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"},
	'M': {"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"},
	'N': {"#...#", "##..#", "#.#.#", "#..##", "#...#", "#...#", "#...#"},
	'P': {"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."},
	'R': {"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"},
	'T': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'U': {"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'V': {"#...#", "#...#", "#...#", "#...#", ".#.#.", ".#.#.", "..#.."},
	'W': {"#...#", "#...#", "#...#", "#.#.#", "#.#.#", "##.##", "#...#"},
	'X': {"#...#", ".#.#.", "..#..", "..#..", "..#..", ".#.#.", "#...#"},
	'Y': {"#...#", ".#.#.", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'3': {"####.", "....#", "....#", ".###.", "....#", "....#", "####."},
	'4': {"#..#.", "#..#.", "#..#.", "#####", "...#.", "...#.", "...#."},
	'6': {".###.", "#....", "#....", "####.", "#...#", "#...#", ".###."},
	'7': {"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
	'9': {".###.", "#...#", "#...#", ".####", "....#", "....#", ".###."},
}

// Render draws the code. The result is a PNG of about 260x90 pixels.
func Render(code string) ([]byte, error) {
	const scale = 7
	const pad = 16
	w := pad*2 + len(code)*(5*scale+10)
	h := 7*scale + pad*2 + 12
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.RGBA{0x2b, 0x2d, 0x31, 0xff}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, bg)
		}
	}
	// speckle
	for i := 0; i < w*h/12; i++ {
		c := uint8(0x40 + mrand.IntN(0x40))
		img.Set(mrand.IntN(w), mrand.IntN(h), color.RGBA{c, c, c + 8, 0xff})
	}
	palette := []color.RGBA{
		{0xf2, 0xb8, 0x4b, 0xff}, {0x7c, 0xd4, 0xfd, 0xff}, {0xa7, 0xf3, 0xd0, 0xff},
		{0xf9, 0xa8, 0xd4, 0xff}, {0xfd, 0xe6, 0x8a, 0xff},
	}
	x0 := pad
	for i := 0; i < len(code); i++ {
		g, ok := glyphs[code[i]]
		if !ok {
			continue
		}
		col := palette[mrand.IntN(len(palette))]
		dy := pad + mrand.IntN(12)
		shear := mrand.Float64()*0.3 - 0.15
		for row := 0; row < 7; row++ {
			for colI := 0; colI < 5; colI++ {
				if g[row][colI] != '#' {
					continue
				}
				for py := 0; py < scale; py++ {
					for px := 0; px < scale; px++ {
						if mrand.IntN(14) == 0 {
							continue // holes make the strokes harder to segment
						}
						yy := dy + row*scale + py
						xx := x0 + colI*scale + px + int(shear*float64(row*scale+py-3*scale))
						if xx >= 0 && xx < w && yy >= 0 && yy < h {
							img.Set(xx, yy, col)
						}
					}
				}
			}
		}
		x0 += 5*scale + 10
	}
	// strike lines across the glyphs
	for n := 0; n < 3; n++ {
		y := float64(pad + mrand.IntN(7*scale))
		slope := mrand.Float64()*0.4 - 0.2
		c := palette[mrand.IntN(len(palette))]
		for x := 0; x < w; x++ {
			yy := int(y + slope*float64(x))
			for t := 0; t < 2; t++ {
				if yy+t >= 0 && yy+t < h {
					img.Set(x, yy+t, c)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
