package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

// Star ratings in a raw's own XMP packet.
//
// A Sony ARW keeps an XMP packet in IFD0 (tag 0x02BC, a fixed 4096-byte
// block): the RDF content, then whitespace padding, then the
// <?xpacket end='w'?> trailer. The padding exists so that tools can edit the
// packet in place (XMP spec part 3, 1.1.2). EmbedRating does exactly that:
// it rewrites bytes inside the packet's own range, taking or giving back
// padding so the packet keeps its length. No TIFF offset moves, and every
// byte outside the packet (IFDs, MakerNote, SubIFDs, previews, raw data)
// stays identical.

const tagXMP = 0x02BC

// Namespaces. Keeper keeps the camera's own rating under its namespace so
// the original in-camera rating is never lost when Keeper's replaces it.
const (
	nsXMP    = "http://ns.adobe.com/xap/1.0/"
	nsKeeper = "https://github.com/geilername/keeper/ns/1.0/"
)

// ErrCannotEmbed is behind every reason EmbedRating refuses a file (as
// opposed to a read or write failure). Nothing has been written then.
var ErrCannotEmbed = errors.New("media: cannot embed rating")

var (
	// ErrNoXMP means the file has no XMP packet in IFD0 to edit.
	ErrNoXMP = fmt.Errorf("%w: no XMP packet in IFD0", ErrCannotEmbed)
	// ErrXMPFull means the packet has too little padding for the rating:
	// writing it would mean growing the packet and moving TIFF offsets.
	ErrXMPFull = fmt.Errorf("%w: XMP packet has too little padding", ErrCannotEmbed)
	// ErrXMPReadOnly means the packet says end='r': it must not be edited.
	ErrXMPReadOnly = fmt.Errorf("%w: XMP packet is marked read-only", ErrCannotEmbed)
	// ErrXMPMalformed means the packet is not a well-formed XMP packet.
	ErrXMPMalformed = fmt.Errorf("%w: malformed XMP packet", ErrCannotEmbed)
	// ErrNotTIFF means the file is not a TIFF-based raw at all.
	ErrNotTIFF = fmt.Errorf("%w: not a TIFF-based raw", ErrCannotEmbed)
)

// EmbedReason is a short, metric-friendly name for why EmbedRating refused
// err ("no_xmp", "xmp_full", …), or "" when err is not a refusal.
func EmbedReason(err error) string {
	switch {
	case errors.Is(err, ErrNoXMP):
		return "no_xmp"
	case errors.Is(err, ErrXMPFull):
		return "xmp_full"
	case errors.Is(err, ErrXMPReadOnly):
		return "xmp_read_only"
	case errors.Is(err, ErrXMPMalformed):
		return "xmp_malformed"
	case errors.Is(err, ErrNotTIFF):
		return "not_tiff"
	case errors.Is(err, ErrCannotEmbed):
		return "other"
	}
	return ""
}

// xmpPacket is where the packet sits in the file and how it splits.
type xmpPacket struct {
	offset, length int64
	data           []byte
	contentEnd     int // after </x:xmpmeta>
	trailerStart   int // at <?xpacket end=
}

var (
	reTrailer   = regexp.MustCompile(`<\?xpacket\s+end\s*=\s*['"]([rw])['"]\s*\?>`)
	reRatingEl  = regexp.MustCompile(`<xmp:Rating>\s*(-?\d+)\s*</xmp:Rating>`)
	reRatingAt  = regexp.MustCompile(`\sxmp:Rating\s*=\s*(['"])(-?\d+)(['"])`)
	reCameraEl  = regexp.MustCompile(`<keeper:CameraRating>`)
	reXMPDesc   = regexp.MustCompile(`<rdf:Description\b[^>]*\sxmlns:xmp\s*=\s*['"]` + regexp.QuoteMeta(nsXMP) + `['"][^>]*>`)
	reRDFEnd    = regexp.MustCompile(`</rdf:RDF>`)
	reXMPMetaNS = regexp.MustCompile(`xmlns:xmp\s*=\s*['"]([^'"]*)['"]`)
)

// findXMP locates and splits IFD0's XMP packet.
func findXMP(r io.ReaderAt, size int64) (*xmpPacket, error) {
	t, err := newTIFF(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNotTIFF, err)
	}
	entries, _, ok := t.readIFD(t.ifd0)
	if !ok {
		return nil, fmt.Errorf("%w: unreadable IFD0", ErrXMPMalformed)
	}
	for _, e := range entries {
		if e.tag != tagXMP || (e.typ != 1 && e.typ != 7) { // BYTE or UNDEFINED
			continue
		}
		if e.count <= 4 || e.count > 1<<20 {
			return nil, ErrXMPMalformed
		}
		p := &xmpPacket{offset: int64(t.bo.Uint32(e.raw[:])), length: int64(e.count)}
		if p.offset < 8 || p.offset+p.length > size {
			return nil, ErrXMPMalformed
		}
		p.data = make([]byte, p.length)
		if _, err := r.ReadAt(p.data, p.offset); err != nil {
			return nil, fmt.Errorf("media: read XMP: %w", err)
		}
		end := bytes.Index(p.data, []byte("</x:xmpmeta>"))
		tr := reTrailer.FindSubmatchIndex(p.data)
		if !bytes.HasPrefix(bytes.TrimLeft(p.data, "\x00 \t\r\n"), []byte("<?xpacket begin")) ||
			end < 0 || tr == nil || tr[0] < end {
			return nil, ErrXMPMalformed
		}
		p.contentEnd = end + len("</x:xmpmeta>")
		p.trailerStart = tr[0]
		if string(p.data[tr[2]:tr[3]]) == "r" {
			return p, ErrXMPReadOnly
		}
		for _, c := range p.data[p.contentEnd:p.trailerStart] {
			if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				return nil, ErrXMPMalformed // something between the meta and the trailer
			}
		}
		return p, nil
	}
	return nil, ErrNoXMP
}

// XMPRating returns the xmp:Rating in the file's XMP packet; ok is false
// when the packet has none.
func XMPRating(r io.ReaderAt, size int64) (rating int, ok bool, err error) {
	p, err := findXMP(r, size)
	if err != nil && !errors.Is(err, ErrXMPReadOnly) {
		return 0, false, err
	}
	rating, ok = ratingIn(p.data[:p.contentEnd])
	return rating, ok, nil
}

func ratingIn(content []byte) (int, bool) {
	if m := reRatingEl.FindSubmatch(content); m != nil {
		n, err := strconv.Atoi(string(m[1]))
		return n, err == nil
	}
	if m := reRatingAt.FindSubmatch(content); m != nil {
		n, err := strconv.Atoi(string(m[2]))
		return n, err == nil
	}
	return 0, false
}

// EmbedRating copies the TIFF-based raw src to w with xmp:Rating set to
// rating (0..5) in its XMP packet. Keeper's rating wins: an existing
// xmp:Rating is replaced, and the camera's value is kept as
// keeper:CameraRating. The packet keeps its exact length; every byte
// outside it is copied unchanged. The same input and rating always give the
// same output.
//
// When it refuses the file (errors.Is(err, ErrCannotEmbed): no packet, too
// little padding, read-only, malformed, not a TIFF) nothing has been written
// to w, and the caller should upload the original instead.
func EmbedRating(src io.ReaderAt, size int64, rating int, w io.Writer) error {
	if rating < 0 || rating > 5 {
		return fmt.Errorf("%w: rating %d out of range 0..5", ErrCannotEmbed, rating)
	}
	p, err := findXMP(src, size)
	if err != nil {
		return err
	}
	packet, err := p.withRating(rating)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, io.NewSectionReader(src, 0, p.offset)); err != nil {
		return err
	}
	if _, err := w.Write(packet); err != nil {
		return err
	}
	end := p.offset + p.length
	_, err = io.Copy(w, io.NewSectionReader(src, end, size-end))
	return err
}

// withRating returns the edited packet, the same length as p.data.
func (p *xmpPacket) withRating(rating int) ([]byte, error) {
	content := append([]byte(nil), p.data[:p.contentEnd]...)
	if m := reXMPMetaNS.FindSubmatch(content); m != nil && string(m[1]) != nsXMP {
		return nil, ErrXMPMalformed // "xmp:" bound to another namespace
	}
	val := []byte(strconv.Itoa(rating))
	camera, hadCamera := ratingIn(content)

	switch {
	case reRatingEl.Match(content):
		m := reRatingEl.FindSubmatchIndex(content)
		content = splice(content, m[2], m[3], val)
	case reRatingAt.Match(content):
		m := reRatingAt.FindSubmatchIndex(content)
		content = splice(content, m[4], m[5], val)
	default:
		el := []byte("\n  <xmp:Rating>" + string(val) + "</xmp:Rating>")
		if m := reXMPDesc.FindIndex(content); m != nil && content[m[1]-2] != '/' {
			content = splice(content, m[1], m[1], el) // into the existing xmp Description
		} else if !insertDescription(&content, "xmlns:xmp='"+nsXMP+"'", "<xmp:Rating>"+string(val)+"</xmp:Rating>") {
			return nil, ErrXMPMalformed
		}
	}
	// Keep the camera's rating once; a file Keeper already rated keeps the
	// first one it recorded.
	if hadCamera && !reCameraEl.Match(content) {
		if !insertDescription(&content, "xmlns:keeper='"+nsKeeper+"'",
			"<keeper:CameraRating>"+strconv.Itoa(camera)+"</keeper:CameraRating>") {
			return nil, ErrXMPMalformed
		}
	}

	pad := p.data[p.contentEnd:p.trailerStart]
	grow := len(content) - p.contentEnd
	// Keep the newline that ends the meta block, and at least one byte
	// of padding, so the packet stays well-formed and editable.
	if grow > len(pad)-2 {
		return nil, ErrXMPFull
	}
	var newPad []byte
	if grow >= 0 {
		newPad = append([]byte{pad[0]}, pad[1+grow:]...)
	} else {
		newPad = append(append([]byte{pad[0]}, bytes.Repeat([]byte{' '}, -grow)...), pad[1:]...)
	}
	out := make([]byte, 0, len(p.data))
	out = append(out, content...)
	out = append(out, newPad...)
	out = append(out, p.data[p.trailerStart:]...)
	if len(out) != len(p.data) {
		return nil, fmt.Errorf("%w: edit changed the packet length (%d → %d)", ErrCannotEmbed, len(p.data), len(out))
	}
	return out, nil
}

// insertDescription adds an rdf:Description with one property before
// </rdf:RDF>, in the camera's own indentation style.
func insertDescription(content *[]byte, xmlns, prop string) bool {
	m := reRDFEnd.FindIndex(*content)
	if m == nil {
		return false
	}
	desc := " <rdf:Description rdf:about=''\n  " + xmlns + ">\n  " + prop + "\n </rdf:Description>\n"
	*content = splice(*content, m[0], m[0], []byte(desc))
	return true
}

func splice(b []byte, from, to int, with []byte) []byte {
	out := make([]byte, 0, len(b)-(to-from)+len(with))
	out = append(out, b[:from]...)
	out = append(out, with...)
	return append(out, b[to:]...)
}
