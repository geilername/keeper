package media

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

// wellFormed parses the whole packet as XML and returns the text of every
// element by its namespace-qualified name ("ns local").
func wellFormed(t *testing.T, pkt []byte) map[string]string {
	t.Helper()
	d := xml.NewDecoder(bytes.NewReader(pkt))
	vals := map[string]string{}
	var cur string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return vals
		}
		if err != nil {
			t.Fatalf("packet is not well-formed XML: %v\n%s", err, pkt)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			cur = tk.Name.Space + " " + tk.Name.Local
		case xml.CharData:
			if v := strings.TrimSpace(string(tk)); v != "" && cur != "" {
				vals[cur] = v
			}
		case xml.EndElement:
			cur = ""
		}
	}
}

const xmpSize = 4096

// xmpPacketOf builds a camera-style packet of exactly n bytes: the content,
// newline, space padding in 100-column lines, and the writable trailer.
func xmpPacketOf(t *testing.T, rdf string, n int, end string) []byte {
	t.Helper()
	head := "<?xpacket begin='\xef\xbb\xbf' id='W5M0MpCehiHzreSzNTczkc9d'?>\n" +
		"<x:xmpmeta xmlns:x='adobe:ns:meta/' x:xmptk=''>\n" +
		"<rdf:RDF xmlns:rdf='http://www.w3.org/1999/02/22-rdf-syntax-ns#'>\n" + rdf +
		"</rdf:RDF>\n</x:xmpmeta>\n"
	trailer := "<?xpacket end='" + end + "'?>"
	padN := n - len(head) - len(trailer)
	if padN < 0 {
		t.Fatalf("content too long for %d bytes", n)
	}
	var pad strings.Builder
	for i := 0; i < padN; i++ {
		if i%101 == 100 {
			pad.WriteByte('\n')
		} else {
			pad.WriteByte(' ')
		}
	}
	return []byte(head + pad.String() + trailer)
}

const (
	sonyRDF = ` <rdf:Description rdf:about=''
  xmlns:xmp='http://ns.adobe.com/xap/1.0/'>
  <xmp:Rating>2</xmp:Rating>
 </rdf:Description>
 <rdf:Description rdf:about=''
  xmlns:xmpMM='http://ns.adobe.com/xap/1.0/mm/'>
  <xmpMM:DocumentID>urn:uuid:3f74e78b-07ab-4691-a02b-04cb946d316f</xmpMM:DocumentID>
  <xmpMM:InstanceID>urn:uuid:dbe67294-642c-4f13-87d7-d52284fd60c6</xmpMM:InstanceID>
 </rdf:Description>
`
	noRatingRDF = ` <rdf:Description rdf:about=''
  xmlns:xmp='http://ns.adobe.com/xap/1.0/'>
  <xmp:CreatorTool>ILCE-7M5</xmp:CreatorTool>
 </rdf:Description>
`
	mmOnlyRDF = ` <rdf:Description rdf:about=''
  xmlns:xmpMM='http://ns.adobe.com/xap/1.0/mm/'>
  <xmpMM:DocumentID>urn:uuid:3f74e78b-07ab-4691-a02b-04cb946d316f</xmpMM:DocumentID>
 </rdf:Description>
`
)

// layout of the synthetic raw: everything outside the packet must survive.
type rawLayout struct {
	file                  []byte
	xmpOff                int
	makerOff, makerLen    int
	stripOff, stripLen    int
	subIFDOff, exifIFDOff int
}

// buildRaw makes a small ARW-shaped TIFF: IFD0 with the XMP packet (when
// packet is non-nil), an EXIF IFD with a MakerNote, and a SubIFD with a
// strip of "raw data".
func buildRaw(t *testing.T, bo binary.ByteOrder, packet []byte) rawLayout {
	t.Helper()
	var l rawLayout
	b := make([]byte, 8)
	if bo == binary.LittleEndian {
		copy(b, "II*\x00")
	} else {
		copy(b, "MM\x00*")
	}
	bo.PutUint32(b[4:], 8)

	type ent struct {
		tag, typ uint16
		count    uint32
		val      uint32
	}
	ifd := func(at int, es []ent) []byte {
		out := make([]byte, 2+12*len(es)+4)
		bo.PutUint16(out, uint16(len(es)))
		for i, e := range es {
			p := out[2+12*i:]
			bo.PutUint16(p, e.tag)
			bo.PutUint16(p[2:], e.typ)
			bo.PutUint32(p[4:], e.count)
			bo.PutUint32(p[8:], e.val)
		}
		return out
	}

	n0 := 4
	if packet == nil {
		n0 = 3
	}
	ifd0Len := 2 + 12*n0 + 4
	l.xmpOff = 8 + ifd0Len
	afterXMP := l.xmpOff + len(packet)
	l.exifIFDOff = afterXMP
	exifLen := 2 + 12*1 + 4
	l.makerOff = l.exifIFDOff + exifLen
	maker := []byte("SONY DSC \x00\x00\x00" + strings.Repeat("\x5a\xa5makernote", 40))
	l.makerLen = len(maker)
	l.subIFDOff = l.makerOff + l.makerLen
	subLen := 2 + 12*2 + 4
	l.stripOff = l.subIFDOff + subLen
	strip := bytes.Repeat([]byte{0x12, 0x34, 0x56, 0x78, 0x9a}, 2000)
	l.stripLen = len(strip)

	es := []ent{{0x0100, 3, 1, 6000}}
	if packet != nil {
		es = append(es, ent{tagXMP, 1, uint32(len(packet)), uint32(l.xmpOff)})
	}
	es = append(es, ent{tagSubIFDs, 4, 1, uint32(l.subIFDOff)}, ent{tagExifIFD, 4, 1, uint32(l.exifIFDOff)})
	b = append(b, ifd(8, es)...)
	b = append(b, packet...)
	b = append(b, ifd(l.exifIFDOff, []ent{{0x927C, 7, uint32(l.makerLen), uint32(l.makerOff)}})...)
	b = append(b, maker...)
	b = append(b, ifd(l.subIFDOff, []ent{{tagStripOffsets, 4, 1, uint32(l.stripOff)}, {tagStripCounts, 4, 1, uint32(l.stripLen)}})...)
	b = append(b, strip...)
	l.file = b
	return l
}

func embed(t *testing.T, file []byte, rating int) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := EmbedRating(bytes.NewReader(file), int64(len(file)), rating, &out)
	if err != nil && out.Len() != 0 {
		t.Fatalf("error %v but %d bytes written", err, out.Len())
	}
	return out.Bytes(), err
}

// sameOutside asserts every byte outside [off, off+n) is unchanged and the
// file length too.
func sameOutside(t *testing.T, a, b []byte, off, n int) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("length %d → %d", len(a), len(b))
	}
	if !bytes.Equal(a[:off], b[:off]) || !bytes.Equal(a[off+n:], b[off+n:]) {
		t.Fatal("bytes outside the XMP packet changed")
	}
}

func packetText(b []byte, l rawLayout) string { return string(b[l.xmpOff : l.xmpOff+xmpSize]) }

func TestEmbedRatingReplacesCameraRating(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		l := buildRaw(t, bo, xmpPacketOf(t, sonyRDF, xmpSize, "w"))
		out, err := embed(t, l.file, 5)
		if err != nil {
			t.Fatal(err)
		}
		sameOutside(t, l.file, out, l.xmpOff, xmpSize)
		// Spelled out for the parts a raw converter cares about most.
		if !bytes.Equal(l.file[l.makerOff:l.makerOff+l.makerLen], out[l.makerOff:l.makerOff+l.makerLen]) ||
			!bytes.Equal(l.file[l.subIFDOff:l.stripOff+l.stripLen], out[l.subIFDOff:l.stripOff+l.stripLen]) {
			t.Fatal("MakerNote or SubIFD changed")
		}
		pkt := packetText(out, l)
		if !strings.Contains(pkt, "<xmp:Rating>5</xmp:Rating>") || strings.Contains(pkt, "<xmp:Rating>2</xmp:Rating>") {
			t.Fatalf("rating not replaced:\n%s", pkt)
		}
		if !strings.Contains(pkt, "xmlns:keeper='"+nsKeeper+"'") || !strings.Contains(pkt, "<keeper:CameraRating>2</keeper:CameraRating>") {
			t.Fatalf("camera rating not kept:\n%s", pkt)
		}
		for _, keep := range []string{"xmpMM:DocumentID>urn:uuid:3f74e78b", "xmpMM:InstanceID>urn:uuid:dbe67294", "<?xpacket end='w'?>"} {
			if !strings.Contains(pkt, keep) {
				t.Fatalf("lost %q", keep)
			}
		}
		if !strings.HasSuffix(pkt, "<?xpacket end='w'?>") {
			t.Fatal("trailer moved")
		}
		vals := wellFormed(t, out[l.xmpOff:l.xmpOff+xmpSize])
		if vals[nsXMP+" Rating"] != "5" || vals[nsKeeper+" CameraRating"] != "2" {
			t.Fatalf("parsed values %v", vals)
		}
		if r, ok, err := XMPRating(bytes.NewReader(out), int64(len(out))); err != nil || !ok || r != 5 {
			t.Fatalf("read back %d %v %v", r, ok, err)
		}
		// The preview walker still finds the same structure.
		if c1, c2 := mustTIFF(t, l.file).candidates(), mustTIFF(t, out).candidates(); len(c1) != len(c2) {
			t.Fatalf("IFD walk differs: %v vs %v", c1, c2)
		}
	}
}

func mustTIFF(t *testing.T, b []byte) *tiff {
	t.Helper()
	tf, err := newTIFF(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return tf
}

func TestEmbedRatingIsDeterministicAndKeepsFirstCameraRating(t *testing.T) {
	l := buildRaw(t, binary.LittleEndian, xmpPacketOf(t, sonyRDF, xmpSize, "w"))
	a, err := embed(t, l.file, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := embed(t, l.file, 4)
	if !bytes.Equal(a, b) {
		t.Fatal("same input and rating gave different bytes")
	}
	// Re-rating an already rated copy: Keeper's new rating wins, the
	// camera's original 2 stays recorded once.
	c, err := embed(t, a, 1)
	if err != nil {
		t.Fatal(err)
	}
	pkt := packetText(c, l)
	if !strings.Contains(pkt, "<xmp:Rating>1</xmp:Rating>") || strings.Count(pkt, "<keeper:CameraRating>") != 1 ||
		!strings.Contains(pkt, "<keeper:CameraRating>2</keeper:CameraRating>") {
		t.Fatalf("re-rated packet:\n%s", pkt)
	}
	sameOutside(t, l.file, c, l.xmpOff, xmpSize)
}

func TestEmbedRatingInsertsWhenAbsent(t *testing.T) {
	for name, rdf := range map[string]string{"into xmp Description": noRatingRDF, "new Description": mmOnlyRDF} {
		t.Run(name, func(t *testing.T) {
			l := buildRaw(t, binary.LittleEndian, xmpPacketOf(t, rdf, xmpSize, "w"))
			out, err := embed(t, l.file, 3)
			if err != nil {
				t.Fatal(err)
			}
			sameOutside(t, l.file, out, l.xmpOff, xmpSize)
			pkt := packetText(out, l)
			if strings.Count(pkt, "<xmp:Rating>3</xmp:Rating>") != 1 || strings.Contains(pkt, "CameraRating") {
				t.Fatalf("packet:\n%s", pkt)
			}
			if rdf == noRatingRDF && strings.Count(pkt, "xmlns:xmp=") != 1 {
				t.Fatal("added a second xmp Description instead of using the camera's")
			}
			if v := wellFormed(t, out[l.xmpOff:l.xmpOff+xmpSize])[nsXMP+" Rating"]; v != "3" {
				t.Fatalf("parsed rating %q", v)
			}
			if r, ok, _ := XMPRating(bytes.NewReader(out), int64(len(out))); !ok || r != 3 {
				t.Fatalf("read back %d %v", r, ok)
			}
		})
	}
}

func TestEmbedRatingRefusesWithoutRoom(t *testing.T) {
	// No packet at all.
	l := buildRaw(t, binary.LittleEndian, nil)
	if _, err := embed(t, l.file, 5); !errors.Is(err, ErrNoXMP) {
		t.Fatalf("no packet: %v", err)
	}
	// A packet whose content fills it: the camera rating needs room for
	// keeper:CameraRating, and there is only a byte or two of padding.
	tight := tightPacket(t, sonyRDF)
	l = buildRaw(t, binary.LittleEndian, tight)
	if _, err := embed(t, l.file, 5); !errors.Is(err, ErrXMPFull) {
		t.Fatalf("full packet: %v", err)
	}
	// Read-only packet.
	l = buildRaw(t, binary.LittleEndian, xmpPacketOf(t, sonyRDF, xmpSize, "r"))
	if _, err := embed(t, l.file, 5); !errors.Is(err, ErrXMPReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
	// Not a TIFF.
	if _, err := embed(t, []byte("\xff\xd8\xff\xe0 a jpeg"), 5); !errors.Is(err, ErrNotTIFF) || EmbedReason(err) != "not_tiff" {
		t.Fatalf("jpeg: %v", err)
	}
	for _, e := range []error{ErrNoXMP, ErrXMPFull, ErrXMPReadOnly, ErrXMPMalformed} {
		if !errors.Is(e, ErrCannotEmbed) || EmbedReason(e) == "" {
			t.Errorf("%v is not a refusal", e)
		}
	}
	if _, err := embed(t, buildRaw(t, binary.LittleEndian, xmpPacketOf(t, sonyRDF, xmpSize, "w")).file, 6); err == nil {
		t.Fatal("rating 6 accepted")
	}
}

// tightPacket is a packet with 10 bytes of padding: enough to swap a digit,
// not enough for keeper:CameraRating.
func tightPacket(t *testing.T, rdf string) []byte {
	t.Helper()
	big := xmpPacketOf(t, rdf, xmpSize, "w")
	content := big[:bytes.Index(big, []byte("</x:xmpmeta>"))+len("</x:xmpmeta>")]
	return xmpPacketOf(t, rdf, len(content)+10+len("<?xpacket end='w'?>"), "w")
}
