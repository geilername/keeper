// Package media knows the file formats Keeper accepts from the camera and
// turns each into an upright JPEG that the scorer and the console can read.
//
// JPEG files are used as they are. Sony ARW raws are TIFF containers with an
// embedded full-colour JPEG preview; Keeper never demosaics the raw data, it
// scores that preview and keeps the raw as the original for Lightroom.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"path/filepath"
	"strings"
)

// Type is a media type as Keeper reports it in events and HTTP uploads.
type Type string

const (
	JPEG Type = "image/jpeg"
	ARW  Type = "image/x-sony-arw"
)

// TypeOf maps a camera file name to its media type. ok is false for files
// Keeper does not ingest.
func TypeOf(name string) (t Type, ok bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return JPEG, true
	case ".arw":
		return ARW, true
	}
	return "", false
}

// Ext is the extension Keeper stores an original under.
func (t Type) Ext() string {
	if t == ARW {
		return ".arw"
	}
	return ".jpg"
}

// ErrNoPreview means a raw file holds no decodable JPEG preview.
var ErrNoPreview = errors.New("media: no embedded JPEG preview")

// EmbeddedPreview returns the largest JPEG embedded in a TIFF-based raw file
// (ARW; DNG and NEF use the same structure). It walks IFD0's chain, every
// SubIFD and the EXIF IFD, and considers both JPEGInterchangeFormat pointers
// and single-strip JPEG-compressed images. Candidates must decode as JPEG;
// the one with the most pixels wins.
func EmbeddedPreview(r io.ReaderAt, size int64) ([]byte, error) {
	t, err := newTIFF(r, size)
	if err != nil {
		return nil, err
	}
	var best []byte
	bestPx := 0
	for _, c := range t.candidates() {
		if c.length < 4 || c.length > 64<<20 || c.offset+c.length > size {
			continue
		}
		buf := make([]byte, c.length)
		if _, err := r.ReadAt(buf, c.offset); err != nil {
			continue
		}
		if buf[0] != 0xFF || buf[1] != 0xD8 {
			continue
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(buf))
		if err != nil {
			continue // e.g. a lossless-JPEG raw strip: not a preview
		}
		if px := cfg.Width * cfg.Height; px > bestPx {
			best, bestPx = buf, px
		}
	}
	if best == nil {
		return nil, ErrNoPreview
	}
	return best, nil
}

// ── minimal TIFF walker ──────────────────────────────────────

const (
	tagCompression  = 0x0103
	tagStripOffsets = 0x0111
	tagStripCounts  = 0x0117
	tagSubIFDs      = 0x014A
	tagJPEGOffset   = 0x0201
	tagJPEGLength   = 0x0202
	tagExifIFD      = 0x8769
	maxIFDs         = 64
	maxEntries      = 4096
)

type tiff struct {
	r    io.ReaderAt
	size int64
	bo   binary.ByteOrder
	ifd0 int64
}

type span struct{ offset, length int64 }

func newTIFF(r io.ReaderAt, size int64) (*tiff, error) {
	var h [8]byte
	if _, err := r.ReadAt(h[:], 0); err != nil {
		return nil, fmt.Errorf("media: read TIFF header: %w", err)
	}
	t := &tiff{r: r, size: size}
	switch {
	case h[0] == 'I' && h[1] == 'I' && h[2] == 42 && h[3] == 0:
		t.bo = binary.LittleEndian
	case h[0] == 'M' && h[1] == 'M' && h[2] == 0 && h[3] == 42:
		t.bo = binary.BigEndian
	default:
		return nil, errors.New("media: not a TIFF-based raw file")
	}
	t.ifd0 = int64(t.bo.Uint32(h[4:]))
	return t, nil
}

type entry struct {
	tag, typ uint16
	count    uint32
	raw      [4]byte // value or offset
}

// values reads an entry's SHORT or LONG values (inline or at its offset).
func (t *tiff) values(e entry) []int64 {
	width := map[uint16]int64{3: 2, 4: 4, 13: 4}[e.typ] // SHORT, LONG, IFD
	if width == 0 || e.count == 0 || e.count > 4096 {
		return nil
	}
	n := int64(e.count)
	data := e.raw[:]
	if n*width > 4 {
		off := int64(t.bo.Uint32(e.raw[:]))
		if off < 0 || off+n*width > t.size {
			return nil
		}
		data = make([]byte, n*width)
		if _, err := t.r.ReadAt(data, off); err != nil {
			return nil
		}
	}
	out := make([]int64, n)
	for i := range out {
		if width == 2 {
			out[i] = int64(t.bo.Uint16(data[i*2:]))
		} else {
			out[i] = int64(t.bo.Uint32(data[i*4:]))
		}
	}
	return out
}

func (t *tiff) readIFD(off int64) (entries []entry, next int64, ok bool) {
	var cnt [2]byte
	if off <= 0 || off+2 > t.size {
		return nil, 0, false
	}
	if _, err := t.r.ReadAt(cnt[:], off); err != nil {
		return nil, 0, false
	}
	n := int64(t.bo.Uint16(cnt[:]))
	if n == 0 || n > maxEntries || off+2+n*12+4 > t.size {
		return nil, 0, false
	}
	buf := make([]byte, n*12+4)
	if _, err := t.r.ReadAt(buf, off+2); err != nil {
		return nil, 0, false
	}
	entries = make([]entry, n)
	for i := range entries {
		b := buf[i*12:]
		entries[i] = entry{tag: t.bo.Uint16(b), typ: t.bo.Uint16(b[2:]), count: t.bo.Uint32(b[4:])}
		copy(entries[i].raw[:], b[8:12])
	}
	return entries, int64(t.bo.Uint32(buf[n*12:])), true
}

// candidates walks every reachable IFD once and collects JPEG spans.
func (t *tiff) candidates() []span {
	var out []span
	seen := map[int64]bool{}
	queue := []int64{t.ifd0}
	for len(queue) > 0 && len(seen) < maxIFDs {
		off := queue[0]
		queue = queue[1:]
		if seen[off] {
			continue
		}
		seen[off] = true
		entries, next, ok := t.readIFD(off)
		if !ok {
			continue
		}
		if next != 0 {
			queue = append(queue, next)
		}
		var jpegOff, jpegLen, compression int64
		var strips, counts []int64
		for _, e := range entries {
			v := t.values(e)
			if len(v) == 0 {
				continue
			}
			switch e.tag {
			case tagJPEGOffset:
				jpegOff = v[0]
			case tagJPEGLength:
				jpegLen = v[0]
			case tagCompression:
				compression = v[0]
			case tagStripOffsets:
				strips = v
			case tagStripCounts:
				counts = v
			case tagSubIFDs, tagExifIFD:
				queue = append(queue, v...)
			}
		}
		if jpegOff > 0 && jpegLen > 0 {
			out = append(out, span{jpegOff, jpegLen})
		}
		if (compression == 6 || compression == 7) && len(strips) == 1 && len(counts) == 1 {
			out = append(out, span{strips[0], counts[0]})
		}
	}
	return out
}

// ── orientation ─────────────────────────────────────────────

// Upright applies an EXIF orientation (1–8) so the image displays as the
// camera was held. 0 and 1 return img unchanged.
func Upright(img image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	swap := orientation >= 5
	dw, dh := w, h
	if swap {
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch orientation {
			case 2: // mirrored horizontally
				dx, dy = w-1-x, y
			case 3: // rotated 180
				dx, dy = w-1-x, h-1-y
			case 4: // mirrored vertically
				dx, dy = x, h-1-y
			case 5: // transposed
				dx, dy = y, x
			case 6: // rotate 90 clockwise to display
				dx, dy = h-1-y, x
			case 7: // transversed
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 90 counter-clockwise to display
				dx, dy = y, w-1-x
			}
			out.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}
