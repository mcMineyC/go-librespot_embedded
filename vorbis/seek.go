package vorbis

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// SeekIndex maps a position in samples to the byte offset the decoder should
// continue reading from. The decoder discards the page at that offset and
// starts decoding with the next one.
type SeekIndex interface {
	SeekOffset(samples int64) (int64, error)
}

// SeekOffset implements SeekIndex with Spotify's seek table. Offsets are
// relative to the stream returned by ExtractMetadataPage.
func (m *MetadataPage) SeekOffset(samples int64) (int64, error) {
	return m.GetSeekPosition(samples), nil
}

// GranuleSeeker implements SeekIndex for a standard Ogg Vorbis stream by
// searching page granule positions. It lands on the last page that ends at or
// before the target, so playback resumes at most one page early.
type GranuleSeeker struct {
	R io.ReaderAt
	// Size returns the stream size. It may overestimate while the stream is
	// still being written; reads past the real end must return io.EOF.
	Size func() int64
	// Guess, when set, returns an approximate offset for samples. The search
	// then only reads around it instead of bisecting the whole stream, which
	// matters when R blocks until data is available.
	Guess func(samples int64) (int64, error)
}

const (
	seekGuessWindow = 256 << 10
	seekLinearSpan  = 64 << 10
	seekScanBuffer  = 64 << 10
)

func (s *GranuleSeeker) SeekOffset(target int64) (int64, error) {
	size := s.Size()
	lo, hi := int64(0), size

	if s.Guess != nil {
		if g, err := s.Guess(target); err == nil {
			if l := max(0, g-seekGuessWindow); l > 0 {
				if off, gran, _, err := nextGranulePage(s.R, l, size); err == nil && gran <= target {
					lo = off
				}
			}
			if h := g + seekGuessWindow; h < size {
				if _, gran, _, err := nextGranulePage(s.R, h, size); err == nil && gran > target {
					hi = h
				}
			}
		}
	}

	// Invariant: the page at lo (or the start of the stream) ends at or
	// before target, and no page starting at or after hi does.
	for hi-lo > seekLinearSpan {
		mid := lo + (hi-lo)/2
		off, gran, _, err := nextGranulePage(s.R, mid, size)
		if errors.Is(err, io.EOF) || (err == nil && (off >= hi || gran > target)) {
			hi = mid
		} else if err != nil {
			return 0, err
		} else {
			lo = off
		}
	}

	best := lo
	for off := lo; ; {
		pOff, gran, pLen, err := nextPage(s.R, off, size)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return 0, err
		}
		if gran != -1 {
			if gran > target {
				break
			}
			best = pOff
		}
		off = pOff + pLen
	}
	return best, nil
}

// nextGranulePage is nextPage, skipping pages on which no packet ends.
func nextGranulePage(r io.ReaderAt, off, size int64) (int64, int64, int64, error) {
	for {
		pOff, gran, pLen, err := nextPage(r, off, size)
		if err != nil || gran != -1 {
			return pOff, gran, pLen, err
		}
		off = pOff + pLen
	}
}

// nextPage finds the first valid Ogg page starting at or after off. It
// returns its offset, granule position (-1 when no packet ends on it) and
// length, or io.EOF when there is none before size.
func nextPage(r io.ReaderAt, off, size int64) (int64, int64, int64, error) {
	buf := make([]byte, seekScanBuffer)
	for off < size {
		n, rerr := r.ReadAt(buf[:min(int64(len(buf)), size-off)], off)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return 0, 0, 0, rerr
		}
		data := buf[:n]
		for i := 0; i+4 <= len(data); {
			j := bytes.Index(data[i:], []byte("OggS"))
			if j < 0 {
				break
			}
			cand := off + int64(i+j)
			if gran, pLen, ok := checkPage(r, cand); ok {
				return cand, gran, pLen, nil
			}
			i += j + 1
		}
		if rerr != nil || n < 4 {
			break
		}
		// Overlap so a capture pattern split across windows is found.
		off += int64(n - 3)
	}
	return 0, 0, 0, io.EOF
}

// checkPage reports whether a complete page with a valid checksum starts at off.
func checkPage(r io.ReaderAt, off int64) (granule int64, length int64, ok bool) {
	var hdr [27]byte
	if n, _ := r.ReadAt(hdr[:], off); n < len(hdr) {
		return 0, 0, false
	}
	if string(hdr[0:4]) != "OggS" || hdr[4] != 0 {
		return 0, 0, false
	}
	nseg := int(hdr[26])
	segs := make([]byte, nseg)
	if n, _ := r.ReadAt(segs, off+27); n < nseg {
		return 0, 0, false
	}
	bodyLen := 0
	for _, s := range segs {
		bodyLen += int(s)
	}
	page := make([]byte, 27+nseg+bodyLen)
	if n, _ := r.ReadAt(page, off); n < len(page) {
		return 0, 0, false
	}
	want := binary.LittleEndian.Uint32(page[22:26])
	clear(page[22:26])
	if oggCrc(page) != want {
		return 0, 0, false
	}
	return int64(binary.LittleEndian.Uint64(hdr[6:14])), int64(len(page)), true
}
