package main

import (
	"image"
	"image/color"
	"testing"
)

// Decoding is refused past denmaMediaMaxDecode: a 48 MP phone photo is
// processed, a 16-bit PNG of 64 MP isn't.
func TestDenmaDecodeSize(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  image.Config
		ok   bool
	}{
		{"48 MP JPEG", image.Config{ColorModel: color.YCbCrModel, Width: 8000, Height: 6000}, true},
		{"40 MP RGBA PNG", image.Config{ColorModel: color.NRGBAModel, Width: 8000, Height: 5000}, true},
		{"64 MP 16-bit PNG", image.Config{ColorModel: color.NRGBA64Model, Width: 8000, Height: 8000}, false},
		{"100 MP JPEG", image.Config{ColorModel: color.YCbCrModel, Width: 10000, Height: 10000}, false},
		{"100 MP GIF", image.Config{ColorModel: color.Palette{color.Black}, Width: 10000, Height: 10000}, true},
	} {
		if ok := denmaDecodeSize(c.cfg) <= denmaMediaMaxDecode; ok != c.ok {
			t.Errorf("%s: processed %v, want %v (%d MB)", c.name, ok, c.ok, denmaDecodeSize(c.cfg)>>20)
		}
	}
}
