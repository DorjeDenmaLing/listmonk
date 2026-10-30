package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"
)

// A PNG over 1 MB without transparency is saved as a JPEG, with its colour
// profile; a smaller one and an animated one stay PNGs. One over 1 MB with a
// transparent pixel stays a PNG if it can be made smaller than that, and is
// refused if it can't.
func TestDenmaPNGToJPEG(t *testing.T) {
	noisy := func(w, h int, transparent bool) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		r := rand.New(rand.NewPCG(1, 2))
		for i := range img.Pix {
			img.Pix[i] = byte(r.IntN(256))
			if i%4 == 3 {
				img.Pix[i] = 255
			}
		}
		if transparent {
			img.Pix[3] = 0
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	// with adds a chunk after IHDR.
	with := func(b []byte, typ string, data []byte) []byte {
		c := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		c = append(c, typ...)
		c = append(c, data...)
		c = binary.BigEndian.AppendUint32(c, crc32.ChecksumIEEE(c[4:]))
		return append(append(append([]byte{}, b[:33]...), c...), b[33:]...)
	}
	profile := bytes.Repeat([]byte("profile-"), 10000) // 80 KB: two segments
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(profile)
	zw.Close()

	big := with(noisy(800, 800, false), "iCCP", append([]byte("Display P3\x00\x00"), z.Bytes()...))
	if len(big) <= denmaMediaPNGToJPEG {
		t.Fatalf("test image is %d bytes, not over the limit", len(big))
	}
	out, err := denmaOptimize(big)
	if err != nil || !out.jpeg || out.width != 800 || out.height != 800 || len(out.data) >= len(big) {
		t.Fatalf("1.9 MB opaque PNG: jpeg %v, %dx%d, %d bytes (err %v)", out.jpeg, out.width, out.height, len(out.data), err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out.data)); err != nil {
		t.Fatalf("its JPEG doesn't decode: %v", err)
	}
	var got []byte
	denmaJPEGSegments(out.data, func(marker byte, seg []byte) {
		if marker == 0xE2 && bytes.HasPrefix(seg[4:], []byte("ICC_PROFILE\x00")) {
			got = append(got, seg[18:]...)
		}
	})
	if !bytes.Equal(got, profile) {
		t.Errorf("its colour profile is %d bytes, want %d", len(got), len(profile))
	}

	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"small opaque PNG", noisy(300, 300, false)},
		{"large animated PNG", with(noisy(800, 800, false), "acTL", make([]byte, 8))},
	} {
		out, err := denmaOptimize(c.b)
		if err != nil || out.jpeg || out.heavy > 0 {
			t.Errorf("%s: saved as a JPEG %v, refused %v (err %v)", c.name, out.jpeg, out.heavy > 0, err)
		}
	}
	if out, _ := denmaOptimize(with(noisy(800, 800, false), "acTL", make([]byte, 8))); out.data != nil {
		t.Errorf("large animated PNG: changed")
	}

	flat := image.NewNRGBA(image.Rect(0, 0, 800, 800))
	for i := range flat.Pix {
		flat.Pix[i] = 200
	}
	flat.Pix[3] = 0
	var raw bytes.Buffer
	(&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&raw, flat)
	out, err = denmaOptimize(raw.Bytes())
	if err != nil || out.jpeg || out.heavy > 0 || out.data == nil || len(out.data) > denmaMediaPNGToJPEG {
		t.Errorf("%d KB transparent PNG that compresses: jpeg %v, refused %v, %d bytes (err %v)", raw.Len()>>10, out.jpeg, out.heavy > 0, len(out.data), err)
	}

	heavy := noisy(800, 800, true)
	out, err = denmaOptimize(heavy)
	if err != nil || out.jpeg || out.heavy == 0 {
		t.Fatalf("%d KB transparent PNG that doesn't compress: jpeg %v, refused %v (err %v)", len(heavy)>>10, out.jpeg, out.heavy > 0, err)
	}
	if _, _, _, err := (&App{}).denmaMediaStore("a.png", "image/png", out); err == nil {
		t.Errorf("it isn't refused on upload")
	}
}

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
