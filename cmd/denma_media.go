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
//     One over denmaMediaPNGToJPEG without transparency is saved as a JPEG
//     (.jpg), with its colour profile; one with transparency still over it
//     once optimized is refused.
//   - GIF (perhaps animated), animated PNG and SVG are stored as they are.
//
// Anything that can't be read is stored as it is, as listmonk would.
//
// Decoding takes memory by the pixel (listmonk's thumbnail decodes the
// whole image too), so an image whose decoding would take more than
// denmaMediaMaxDecode is refused, from its header, and one upload's images
// are processed at a time (denmaMediaLimit): a few large photos at once
// could otherwise take the server's memory.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"
	"github.com/labstack/echo/v4"
)

const (
	denmaMediaMaxSize   = 1600      // px, the longer side
	denmaMediaQuality   = 82        // JPEG
	denmaMediaMaxDecode = 512 << 20 // bytes an image's decoding may take
	denmaMediaPNGToJPEG = 1 << 20   // bytes: a larger PNG without transparency is saved as a JPEG
)

// denmaMediaTurn is the turn to process an uploaded image: one at a time.
var denmaMediaTurn = make(chan struct{}, 1)

// denmaMediaLimit wraps the media upload: an image (by its extension) too
// large to decode is refused, and images wait for their turn.
func (a *App) denmaMediaLimit(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		file, err := c.FormFile("file")
		if err != nil {
			return next(c) // listmonk says what's wrong
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(file.Filename)), ".")
		if !inArray(ext, imageExts) {
			return next(c)
		}
		if src, err := file.Open(); err == nil {
			cfg, _, err := image.DecodeConfig(src)
			src.Close()
			if err == nil && denmaDecodeSize(cfg) > denmaMediaMaxDecode {
				return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
					"The image is %d x %d pixels, too large to process. Make it smaller (say, 4000 px on its longer side) and upload it again.",
					cfg.Width, cfg.Height))
			}
		}
		select {
		case denmaMediaTurn <- struct{}{}:
		case <-c.Request().Context().Done():
			return c.Request().Context().Err()
		}
		defer func() { <-denmaMediaTurn }()
		return next(c)
	}
}

// denmaDecodeSize is about how much memory decoding an image takes: its
// pixels as decoded, and again as imaging's 8-bit RGBA copy.
func denmaDecodeSize(cfg image.Config) int64 {
	bpp := int64(4)
	switch cfg.ColorModel {
	case color.RGBA64Model, color.NRGBA64Model:
		bpp = 8
	case color.Gray16Model:
		bpp = 2
	case color.GrayModel:
		bpp = 1
	case color.YCbCrModel:
		bpp = 3 // at most, unsubsampled
	default:
		if _, ok := cfg.ColorModel.(color.Palette); ok {
			bpp = 1
		}
	}
	return int64(cfg.Width) * int64(cfg.Height) * (bpp + 4)
}

// denmaMedia is an optimized upload: data is nil to store the upload as it is.
type denmaMedia struct {
	data          []byte
	width, height int
	jpeg          bool  // a PNG saved as a JPEG
	heavy         int64 // bytes of a PNG with transparency still over denmaMediaPNGToJPEG: refused
}

// denmaMediaStore is the name and content type to store an upload under: a
// PNG saved as a JPEG is named .jpg, made unique as listmonk does (if it
// can't be, the upload is stored as it is). A PNG with transparency still
// over denmaMediaPNGToJPEG once optimized is refused.
func (a *App) denmaMediaStore(fName, contentType string, m denmaMedia) (string, string, denmaMedia, error) {
	if m.heavy > 0 {
		return "", "", m, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf(
			"This PNG has transparent parts and would still be %.1f MB at %d px, too large for e-mails. Save it without transparency (it will be stored as a JPEG) or make it smaller, and upload it again.",
			float64(m.heavy)/(1<<20), denmaMediaMaxSize))
	}
	if !m.jpeg {
		return fName, contentType, m, nil
	}
	name := strings.TrimSuffix(fName, filepath.Ext(fName)) + ".jpg"
	if _, err := a.core.GetMedia(0, "", name, a.media); err == nil {
		suffix, err := generateRandomString(6)
		if err != nil {
			return fName, contentType, denmaMedia{}, nil
		}
		name = appendSuffixToFilename(name, suffix)
	}
	return name, "image/jpeg", m, nil
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
	if denmaDecodeSize(cfg) > denmaMediaMaxDecode {
		return denmaMedia{}, nil // refused before (denmaMediaLimit)
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
		if denmaPNGAnimated(orig) {
			return denmaMedia{}, nil
		}
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
		b := img.Bounds()
		smaller := tooBig || buf.Len() < len(orig)*95/100
		stored := len(orig)
		if smaller {
			stored = buf.Len()
		}
		if len(orig) > denmaMediaPNGToJPEG {
			if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
				var j bytes.Buffer
				if err := jpeg.Encode(&j, img, &jpeg.Options{Quality: denmaMediaQuality}); err != nil {
					return denmaMedia{}, err
				}
				if j.Len() < buf.Len() {
					return denmaMedia{data: denmaJPEGWithProfile(j.Bytes(), denmaPNGProfile(orig)), width: b.Dx(), height: b.Dy(), jpeg: true}, nil
				}
			} else if stored > denmaMediaPNGToJPEG {
				return denmaMedia{heavy: int64(stored)}, nil
			}
		}
		if smaller {
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

// denmaJPEGWithProfile adds a colour profile (nil: none) to a JPEG that has
// none, as APP2 ICC_PROFILE segments.
func denmaJPEGWithProfile(enc, profile []byte) []byte {
	const most = 0xFFFF - 2 - 14 // a segment's data, after its length and header
	n := (len(profile) + most - 1) / most
	if n == 0 || n > 255 || len(enc) < 2 {
		return enc
	}
	out := make([]byte, 0, len(enc)+len(profile)+n*18)
	out = append(out, enc[:2]...)
	for i := range n {
		part := profile[i*most : min((i+1)*most, len(profile))]
		out = append(out, 0xFF, 0xE2)
		out = binary.BigEndian.AppendUint16(out, uint16(2+14+len(part)))
		out = append(out, "ICC_PROFILE\x00"...)
		out = append(out, byte(i+1), byte(n))
		out = append(out, part...)
	}
	return append(out, enc[2:]...)
}

// ---- PNG chunks ----

// denmaPNGChunks calls f with each chunk before the image data (IDAT): its
// type and its data.
func denmaPNGChunks(b []byte, f func(typ string, data []byte)) {
	if !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) {
		return
	}
	for i := 8; i+12 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		typ := string(b[i+4 : i+8])
		if typ == "IDAT" || n > len(b)-i-12 {
			return
		}
		f(typ, b[i+8:i+8+n])
		i += 12 + n
	}
}

// denmaPNGAnimated is whether the PNG is animated (APNG).
func denmaPNGAnimated(b []byte) bool {
	animated := false
	denmaPNGChunks(b, func(typ string, _ []byte) {
		animated = animated || typ == "acTL"
	})
	return animated
}

// denmaPNGProfile is the PNG's colour profile (iCCP), or nil.
func denmaPNGProfile(b []byte) []byte {
	var profile []byte
	denmaPNGChunks(b, func(typ string, data []byte) {
		if typ != "iCCP" || profile != nil {
			return
		}
		name := bytes.IndexByte(data, 0) // the profile's name, then the compression method
		if name < 0 || name+2 > len(data) || data[name+1] != 0 {
			return
		}
		r, err := zlib.NewReader(bytes.NewReader(data[name+2:]))
		if err != nil {
			return
		}
		defer r.Close()
		p, err := io.ReadAll(io.LimitReader(r, 4<<20+1))
		if err == nil && len(p) <= 4<<20 {
			profile = p
		}
	})
	return profile
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
