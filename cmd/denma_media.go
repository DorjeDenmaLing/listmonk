package main

// denma: uploaded images are optimized before they're stored (UploadMedia,
// cmd/media.go). Phone photos are often 4-12 MB, 4000 px wide and sideways
// (rotated only by an EXIF tag), with the camera's metadata, including where
// they were taken. Every subscriber who opens an email downloads them.
//
//   - Larger than denmaMediaMaxSize on the longer side: scaled down to fit.
//     An email is about 600 px wide (1200 px on high-density screens).
//   - JPEG: turned upright, metadata (EXIF, XMP, IPTC, comments) removed and
//     the colour profile kept. Re-saved at denmaMediaQuality when that's
//     clearly smaller (or needed, to scale or turn it); otherwise the
//     original image data is kept, without the metadata.
//   - PNG: scaled down, or recompressed (losslessly) when that's smaller.
//   - GIF (perhaps animated) and SVG are stored as they are.
//
// Anything that can't be read is stored as it is, as listmonk would.

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/disintegration/imaging"
)

const (
	denmaMediaMaxSize  = 1600     // px, the longer side
	denmaMediaQuality  = 82       // JPEG
	denmaMediaMaxPixel = 60000000 // larger images aren't decoded (memory)
)

// denmaMedia is an optimized upload: data is nil to store the upload as it is.
type denmaMedia struct {
	data          []byte
	width, height int
}

// reader is what to store.
func (m denmaMedia) reader(src io.ReadSeeker) io.ReadSeeker {
	if m.data == nil {
		return src
	}
	return bytes.NewReader(m.data)
}

// size is the stored image's width and height.
func (m denmaMedia) size(w, h int) (int, int) {
	if m.data == nil {
		return w, h
	}
	return m.width, m.height
}

// denmaOptimizeImage optimizes an uploaded image (ext is its lowercase
// extension). src is left at its start.
func (a *App) denmaOptimizeImage(name, ext string, src io.ReadSeeker) denmaMedia {
	if ext != "jpg" && ext != "jpeg" && ext != "png" {
		return denmaMedia{}
	}
	orig, err := io.ReadAll(src)
	if _, serr := src.Seek(0, io.SeekStart); err != nil || serr != nil {
		return denmaMedia{}
	}
	out, err := denmaOptimize(orig)
	if err != nil {
		a.log.Printf("denma: not optimizing %s: %v", name, err)
		return denmaMedia{}
	}
	if out.data != nil {
		a.log.Printf("denma: optimized %s: %d KB -> %d KB (%dx%d)", name, len(orig)/1024, len(out.data)/1024, out.width, out.height)
	}
	return out
}

func denmaOptimize(orig []byte) (denmaMedia, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(orig))
	if err != nil {
		return denmaMedia{}, err
	}
	if cfg.Width*cfg.Height > denmaMediaMaxPixel {
		return denmaMedia{}, nil
	}
	tooBig := cfg.Width > denmaMediaMaxSize || cfg.Height > denmaMediaMaxSize

	switch format {
	case "jpeg":
		upright := denmaJPEGOrientation(orig) <= 1
		stripped := denmaJPEGStrip(orig)
		img, err := imaging.Decode(bytes.NewReader(orig), imaging.AutoOrientation(true))
		if err != nil {
			return denmaMedia{}, err
		}
		if tooBig {
			img = imaging.Fit(img, denmaMediaMaxSize, denmaMediaMaxSize, imaging.Lanczos)
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: denmaMediaQuality}); err != nil {
			return denmaMedia{}, err
		}
		enc := buf.Bytes()
		if _, cmyk := img.(*image.CMYK); !cmyk { // a CMYK profile doesn't fit the RGB output
			enc = denmaJPEGWithICC(enc, orig)
		}
		b := img.Bounds()
		if tooBig || !upright || len(enc) < len(stripped)*85/100 {
			return denmaMedia{data: enc, width: b.Dx(), height: b.Dy()}, nil
		}
		if len(stripped) < len(orig) {
			return denmaMedia{data: stripped, width: cfg.Width, height: cfg.Height}, nil
		}
		return denmaMedia{}, nil

	case "png":
		img, err := png.Decode(bytes.NewReader(orig))
		if err != nil {
			return denmaMedia{}, err
		}
		if tooBig {
			img = imaging.Fit(img, denmaMediaMaxSize, denmaMediaMaxSize, imaging.Lanczos)
		}
		var buf bytes.Buffer
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
			return denmaMedia{}, err
		}
		if tooBig || buf.Len() < len(orig)*95/100 {
			b := img.Bounds()
			return denmaMedia{data: buf.Bytes(), width: b.Dx(), height: b.Dy()}, nil
		}
	}
	return denmaMedia{}, nil
}

// ---- JPEG segments ----

// denmaJPEGSegments calls f with each marker segment before the image data
// (start of scan): its marker and its bytes, marker included. It returns
// where the image data starts, or -1 if the file isn't laid out as expected.
func denmaJPEGSegments(b []byte, f func(marker byte, seg []byte)) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return -1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return -1
		}
		marker := b[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0xDA { // start of scan: the rest is image data
			return i
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return -1
		}
		f(marker, b[i:i+2+n])
		i += 2 + n
	}
	return -1
}

// denmaJPEGStrip is the JPEG without its metadata: EXIF and XMP (APP1),
// IPTC (APP13) and comments. Anything else, such as the colour profile
// (APP2), stays. It's the original if it can't be read.
func denmaJPEGStrip(b []byte) []byte {
	out := []byte{0xFF, 0xD8}
	sos := denmaJPEGSegments(b, func(marker byte, seg []byte) {
		if marker != 0xE1 && marker != 0xED && marker != 0xFE {
			out = append(out, seg...)
		}
	})
	if sos < 0 {
		return b
	}
	return append(out, b[sos:]...)
}

// denmaJPEGWithICC adds the original's colour profile (APP2 ICC_PROFILE
// segments) to a JPEG that has none (Go's encoder writes none).
func denmaJPEGWithICC(enc, orig []byte) []byte {
	var icc []byte
	denmaJPEGSegments(orig, func(marker byte, seg []byte) {
		if marker == 0xE2 && bytes.HasPrefix(seg[4:], []byte("ICC_PROFILE\x00")) {
			icc = append(icc, seg...)
		}
	})
	if icc == nil || len(enc) < 2 {
		return enc
	}
	out := make([]byte, 0, len(enc)+len(icc))
	out = append(out, enc[:2]...)
	out = append(out, icc...)
	return append(out, enc[2:]...)
}

// denmaJPEGOrientation is the EXIF orientation (1 is upright; 0 if there's none).
func denmaJPEGOrientation(b []byte) int {
	orientation := 0
	denmaJPEGSegments(b, func(marker byte, seg []byte) {
		if marker != 0xE1 || orientation != 0 || !bytes.HasPrefix(seg[4:], []byte("Exif\x00\x00")) {
			return
		}
		t := seg[10:] // the TIFF header
		if len(t) < 8 {
			return
		}
		var bo binary.ByteOrder = binary.BigEndian
		if t[0] == 'I' {
			bo = binary.LittleEndian
		}
		ifd := int(bo.Uint32(t[4:]))
		if ifd+2 > len(t) {
			return
		}
		for n, i := int(bo.Uint16(t[ifd:])), 0; i < n; i++ {
			e := ifd + 2 + i*12
			if e+12 > len(t) {
				return
			}
			if bo.Uint16(t[e:]) == 0x0112 {
				orientation = int(bo.Uint16(t[e+8:]))
				return
			}
		}
	})
	return orientation
}
