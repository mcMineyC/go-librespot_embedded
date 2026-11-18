package vorbis

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Picture is an embedded cover image, written as a METADATA_BLOCK_PICTURE comment.
type Picture struct {
	MimeType    string
	Description string
	Width       uint32
	Height      uint32
	Data        []byte
}

// MetadataBlockPicture returns the Vorbis comment ("METADATA_BLOCK_PICTURE=<base64>")
// carrying p as a FLAC picture block with picture type 3 (front cover).
func (p Picture) MetadataBlockPicture() string {
	var b bytes.Buffer
	be := func(v uint32) { _ = binary.Write(&b, binary.BigEndian, v) }
	be(3) // front cover
	be(uint32(len(p.MimeType)))
	b.WriteString(p.MimeType)
	be(uint32(len(p.Description)))
	b.WriteString(p.Description)
	be(p.Width)
	be(p.Height)
	be(24) // colour depth
	be(0)  // indexed colours (none)
	be(uint32(len(p.Data)))
	b.Write(p.Data)
	return "METADATA_BLOCK_PICTURE=" + base64.StdEncoding.EncodeToString(b.Bytes())
}

const (
	oggFlagContinued = 0x01
	oggFlagBOS       = 0x02
	oggFlagEOS       = 0x04
)

type oggPage struct {
	flags    byte
	granule  uint64
	serial   uint32
	seq      uint32
	segments []byte
	body     []byte
}

func readOggPage(r *bufio.Reader) (*oggPage, error) {
	var hdr [27]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if string(hdr[0:4]) != "OggS" {
		return nil, errors.New("ogg: missing capture pattern")
	}
	if hdr[4] != 0 {
		return nil, fmt.Errorf("ogg: unsupported version %d", hdr[4])
	}

	p := &oggPage{
		flags:    hdr[5],
		granule:  binary.LittleEndian.Uint64(hdr[6:14]),
		serial:   binary.LittleEndian.Uint32(hdr[14:18]),
		seq:      binary.LittleEndian.Uint32(hdr[18:22]),
		segments: make([]byte, hdr[26]),
	}
	if _, err := io.ReadFull(r, p.segments); err != nil {
		return nil, fmt.Errorf("ogg: truncated segment table: %w", err)
	}

	var bodyLen int
	for _, s := range p.segments {
		bodyLen += int(s)
	}
	p.body = make([]byte, bodyLen)
	if _, err := io.ReadFull(r, p.body); err != nil {
		return nil, fmt.Errorf("ogg: truncated page body: %w", err)
	}
	return p, nil
}

func (p *oggPage) writeTo(w io.Writer) error {
	buf := make([]byte, 27+len(p.segments)+len(p.body))
	copy(buf[0:4], "OggS")
	buf[5] = p.flags
	binary.LittleEndian.PutUint64(buf[6:14], p.granule)
	binary.LittleEndian.PutUint32(buf[14:18], p.serial)
	binary.LittleEndian.PutUint32(buf[18:22], p.seq)
	buf[26] = byte(len(p.segments))
	copy(buf[27:], p.segments)
	copy(buf[27+len(p.segments):], p.body)
	binary.LittleEndian.PutUint32(buf[22:26], oggCrc(buf))
	_, err := w.Write(buf)
	return err
}

var oggCrcTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return
}()

func oggCrc(b []byte) uint32 {
	var crc uint32
	for _, v := range b {
		crc = crc<<8 ^ oggCrcTable[byte(crc>>24)^v]
	}
	return crc
}

// pagePackets splits packets into Ogg pages (max 255 lacing values each),
// starting at sequence number seq. All pages get granule position 0 except
// those on which no packet completes, which get -1 as the spec requires.
func pagePackets(serial, seq uint32, packets ...[]byte) []*oggPage {
	type seg struct {
		data []byte
		last bool // last segment of a packet
	}
	var segs []seg
	for _, pkt := range packets {
		for len(pkt) >= 255 {
			segs = append(segs, seg{data: pkt[:255]})
			pkt = pkt[255:]
		}
		segs = append(segs, seg{data: pkt, last: true})
	}

	var pages []*oggPage
	continued := false
	for len(segs) > 0 {
		n := min(len(segs), 255)
		p := &oggPage{serial: serial, seq: seq, granule: ^uint64(0)}
		if continued {
			p.flags |= oggFlagContinued
		}
		for _, s := range segs[:n] {
			p.segments = append(p.segments, byte(len(s.data)))
			p.body = append(p.body, s.data...)
			if s.last {
				p.granule = 0
			}
		}
		continued = !segs[n-1].last
		segs = segs[n:]
		pages = append(pages, p)
		seq++
	}
	return pages
}

func (p *oggPage) size() int64 {
	return int64(27 + len(p.segments) + len(p.body))
}

// readHeaderPackets reads pages until the three Vorbis header packets are complete.
// It returns the identification page as-is, the comment and setup packets, and the
// number of bytes the header pages took.
func readHeaderPackets(r *bufio.Reader) (*oggPage, []byte, []byte, int64, error) {
	idPage, err := readOggPage(r)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("ogg: reading identification page: %w", err)
	}
	if idPage.flags&oggFlagBOS == 0 || len(idPage.body) < 7 || idPage.body[0] != 0x01 || string(idPage.body[1:7]) != "vorbis" {
		return nil, nil, nil, 0, errors.New("ogg: first page is not a Vorbis identification header (was the Spotify metadata page stripped?)")
	}
	headerLen := idPage.size()

	var packets [][]byte
	var cur []byte
	for len(packets) < 2 {
		p, err := readOggPage(r)
		if err != nil {
			return nil, nil, nil, 0, fmt.Errorf("ogg: reading header pages: %w", err)
		}
		if p.serial != idPage.serial {
			return nil, nil, nil, 0, errors.New("ogg: multiplexed streams are not supported")
		}
		headerLen += p.size()

		off := 0
		for _, s := range p.segments {
			cur = append(cur, p.body[off:off+int(s)]...)
			off += int(s)
			if s < 255 {
				packets = append(packets, cur)
				cur = nil
			}
		}
	}
	if len(packets) != 2 || cur != nil {
		return nil, nil, nil, 0, errors.New("ogg: setup header does not end on a page boundary")
	}
	if len(packets[0]) < 7 || packets[0][0] != 0x03 || string(packets[0][1:7]) != "vorbis" {
		return nil, nil, nil, 0, errors.New("ogg: second packet is not a Vorbis comment header")
	}
	return idPage, packets[0], packets[1], headerLen, nil
}

func commentVendor(pkt []byte) string {
	if len(pkt) < 11 {
		return ""
	}
	n := binary.LittleEndian.Uint32(pkt[7:11])
	if uint64(n) > uint64(len(pkt)-11) {
		return ""
	}
	return string(pkt[11 : 11+n])
}

func buildCommentPacket(vendor string, comments []string) []byte {
	var b bytes.Buffer
	le := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteByte(0x03)
	b.WriteString("vorbis")
	le(uint32(len(vendor)))
	b.WriteString(vendor)
	le(uint32(len(comments)))
	for _, c := range comments {
		le(uint32(len(c)))
		b.WriteString(c)
	}
	b.WriteByte(0x01) // framing bit
	return b.Bytes()
}

// WriteTagged copies a standard Ogg Vorbis stream (Spotify metadata page already
// removed) from r to w, replacing its comment header with comments. Comments are
// "KEY=value" strings. Audio pages are copied unchanged except for their sequence
// numbers and checksums, which are rewritten to follow the new header pages.
func WriteTagged(w io.Writer, r io.Reader, comments []string) error {
	return WriteTaggedNotify(w, r, comments, nil)
}

// WriteTaggedNotify is WriteTagged, calling onHeader (when not nil) with the
// header sizes of the input and the output before any audio page is copied.
// Audio pages keep their size, so an input offset past the header maps to
// output offset + dstHeader - srcHeader.
func WriteTaggedNotify(w io.Writer, r io.Reader, comments []string, onHeader func(srcHeader, dstHeader int64)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	bw := bufio.NewWriterSize(w, 64*1024)

	idPage, commentPkt, setupPkt, srcHeader, err := readHeaderPackets(br)
	if err != nil {
		return err
	}

	if err := idPage.writeTo(bw); err != nil {
		return err
	}

	newComment := buildCommentPacket(commentVendor(commentPkt), comments)
	headerPages := pagePackets(idPage.serial, idPage.seq+1, newComment, setupPkt)
	for _, p := range headerPages {
		if err := p.writeTo(bw); err != nil {
			return err
		}
	}

	if onHeader != nil {
		dstHeader := idPage.size()
		for _, p := range headerPages {
			dstHeader += p.size()
		}
		onHeader(srcHeader, dstHeader)
	}

	seq := headerPages[len(headerPages)-1].seq + 1
	for {
		p, err := readOggPage(br)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		p.seq = seq
		seq++
		if err := p.writeTo(bw); err != nil {
			return err
		}

		// Spotify files carry a few padding bytes after the last page; stop at
		// end-of-stream instead of trying to parse them.
		if p.flags&oggFlagEOS != 0 {
			break
		}
	}

	return bw.Flush()
}
