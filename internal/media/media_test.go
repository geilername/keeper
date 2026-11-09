package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeARW lays out a file shaped like a Sony ARW: IFD0 points at a small
// thumbnail, IFD1 (IFD0's next) at a mid preview, and a SubIFD holds a
// JPEG-compressed strip that is the large preview, plus a "lossless raw"
// strip that is not a decodable JPEG.
func fakeARW(t *testing.T, bo binary.ByteOrder) (file []byte, want []byte) {
	thumb := jpegOf(t, 160, 120)
	preview := jpegOf(t, 1616, 1080)
	rawStrip := append([]byte{0xFF, 0xD8, 0xFF, 0xC3}, make([]byte, 400)...) // lossless JPEG marker, undecodable

	var buf bytes.Buffer
	put16 := func(v uint16) { _ = binary.Write(&buf, bo, v) }
	put32 := func(v uint32) { _ = binary.Write(&buf, bo, v) }
	if bo == binary.LittleEndian {
		buf.WriteString("II")
	} else {
		buf.WriteString("MM")
	}
	put16(42)
	put32(8) // IFD0 at 8

	// layout offsets
	const ifd0 = 8
	ifd0Size := 2 + 3*12 + 4
	ifd1 := ifd0 + ifd0Size
	ifd1Size := 2 + 2*12 + 4
	sub := ifd1 + ifd1Size
	subSize := 2 + 3*12 + 4
	sub2 := sub + subSize
	sub2Size := 2 + 3*12 + 4
	data := sub2 + sub2Size
	thumbAt := data
	previewAt := thumbAt + len(thumb)
	rawAt := previewAt + len(preview)

	ent := func(tag, typ uint16, count, val uint32) { put16(tag); put16(typ); put32(count); put32(val) }
	// IFD0: thumbnail pointer + two SubIFDs (stored inline would need 8 bytes, so point at a LONG array)
	put16(3)
	ent(tagJPEGOffset, 4, 1, uint32(thumbAt))
	ent(tagJPEGLength, 4, 1, uint32(len(thumb)))
	ent(tagSubIFDs, 4, 1, uint32(sub)) // one SubIFD; second chained via its next pointer
	put32(uint32(ifd1))
	// IFD1: nothing interesting, only a duplicate pointer at the thumbnail
	put16(2)
	ent(tagJPEGOffset, 4, 1, uint32(thumbAt))
	ent(tagJPEGLength, 4, 1, uint32(len(thumb)))
	put32(0)
	// SubIFD: JPEG-compressed single strip = the large preview
	put16(3)
	ent(tagCompression, 3, 1, uint32(6)<<shiftFor(bo))
	ent(tagStripOffsets, 4, 1, uint32(previewAt))
	ent(tagStripCounts, 4, 1, uint32(len(preview)))
	put32(uint32(sub2))
	// second SubIFD: lossless-JPEG raw strip, must be skipped
	put16(3)
	ent(tagCompression, 3, 1, uint32(7)<<shiftFor(bo))
	ent(tagStripOffsets, 4, 1, uint32(rawAt))
	ent(tagStripCounts, 4, 1, uint32(len(rawStrip)))
	put32(0)

	if buf.Len() != data {
		t.Fatalf("layout off: header+IFDs %d bytes, data planned at %d", buf.Len(), data)
	}
	buf.Write(thumb)
	buf.Write(preview)
	buf.Write(rawStrip)
	return buf.Bytes(), preview
}

// A SHORT value sits in the first two bytes of the 4-byte value field: for
// big-endian that is the high half of the uint32 we write.
func shiftFor(bo binary.ByteOrder) uint {
	if bo == binary.BigEndian {
		return 16
	}
	return 0
}

func TestEmbeddedPreviewPicksLargestDecodableJPEG(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(bo.String(), func(t *testing.T) {
			file, want := fakeARW(t, bo)
			got, err := EmbeddedPreview(bytes.NewReader(file), int64(len(file)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				cfg, _ := jpeg.DecodeConfig(bytes.NewReader(got))
				t.Fatalf("picked a %dx%d JPEG, want the 1616x1080 preview", cfg.Width, cfg.Height)
			}
		})
	}
}

func TestEmbeddedPreviewRejectsGarbage(t *testing.T) {
	for name, file := range map[string][]byte{
		"not tiff":       []byte("hello world, not a raw"),
		"empty":          {},
		"loop":           append([]byte("II*\x00\x08\x00\x00\x00"), 0x00, 0x00), // IFD with zero entries
		"truncated ifd0": []byte("II*\x00\xff\xff\x00\x00"),
	} {
		if _, err := EmbeddedPreview(bytes.NewReader(file), int64(len(file))); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestTypeOf(t *testing.T) {
	for name, want := range map[string]Type{"DSC01234.ARW": ARW, "a.arw": ARW, "b.JPG": JPEG, "c.jpeg": JPEG} {
		if got, ok := TypeOf(name); !ok || got != want {
			t.Errorf("TypeOf(%q) = %q,%v", name, got, ok)
		}
	}
	if _, ok := TypeOf("clip.MP4"); ok {
		t.Error("MP4 must not be accepted")
	}
}

func TestUpright(t *testing.T) {
	// 3x2 image; mark the top-left pixel red.
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	red := color.RGBA{255, 0, 0, 255}
	src.Set(0, 0, red)
	cases := []struct {
		orientation int
		w, h        int
		redX, redY  int
	}{
		{1, 3, 2, 0, 0},
		{3, 3, 2, 2, 1}, // 180: top-left goes bottom-right
		{6, 2, 3, 1, 0}, // camera rotated: top-left goes top-right
		{8, 2, 3, 0, 2}, // other way: top-left goes bottom-left
	}
	for _, c := range cases {
		out := Upright(src, c.orientation)
		if b := out.Bounds(); b.Dx() != c.w || b.Dy() != c.h {
			t.Errorf("orientation %d: size %dx%d, want %dx%d", c.orientation, b.Dx(), b.Dy(), c.w, c.h)
			continue
		}
		if r, _, _, _ := out.At(c.redX, c.redY).RGBA(); r>>8 != 255 {
			t.Errorf("orientation %d: red pixel not at (%d,%d)", c.orientation, c.redX, c.redY)
		}
	}
}
