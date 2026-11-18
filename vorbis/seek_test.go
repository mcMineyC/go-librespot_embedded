package vorbis

import (
	"bytes"
	"testing"
)

// buildStream writes n pages of bodyLen bytes; page i has granule (i+1)*1000,
// except every 7th page, on which no packet ends (granule -1). Bodies contain
// a fake capture pattern to make sure only checksummed pages are accepted.
func buildStream(t *testing.T, n, bodyLen int) ([]byte, []int64, []int64) {
	t.Helper()
	var buf bytes.Buffer
	var offsets, granules []int64
	for i := 0; i < n; i++ {
		body := bytes.Repeat([]byte{byte(i)}, bodyLen)
		copy(body[10:], "OggS\x00\x00garbage")
		segs := make([]byte, 0, bodyLen/255+1)
		for left := bodyLen; ; left -= 255 {
			if left >= 255 {
				segs = append(segs, 255)
				continue
			}
			segs = append(segs, byte(left))
			break
		}
		gran := int64(i+1) * 1000
		if i%7 == 6 {
			gran = -1
		}
		p := &oggPage{granule: uint64(gran), serial: 1, seq: uint32(i), segments: segs, body: body}
		offsets = append(offsets, int64(buf.Len()))
		granules = append(granules, gran)
		if err := p.writeTo(&buf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes(), offsets, granules
}

// expected returns the offset of the last page with a granule <= target.
func expected(offsets, granules []int64, target int64) int64 {
	best := int64(0)
	for i, g := range granules {
		if g == -1 {
			continue
		}
		if g > target {
			break
		}
		best = offsets[i]
	}
	return best
}

func TestGranuleSeeker(t *testing.T) {
	data, offsets, granules := buildStream(t, 400, 3000)
	r := bytes.NewReader(data)
	size := func() int64 { return int64(len(data)) }

	targets := []int64{0, 500, 1000, 1500, 6999, 7000, 123456, 250000, 399999, 400000, 10_000_000}
	for _, target := range targets {
		want := expected(offsets, granules, target)

		plain := &GranuleSeeker{R: r, Size: size}
		if got, err := plain.SeekOffset(target); err != nil || got != want {
			t.Errorf("plain target %d: got %d, %v; want %d", target, got, err, want)
		}

		// A good guess, a guess that is far off and an overestimated size
		// must all give the same answer.
		for _, guess := range []int64{want, 0, int64(len(data)) / 2, int64(len(data))} {
			g := guess
			s := &GranuleSeeker{R: r, Size: func() int64 { return int64(len(data)) + 5000 },
				Guess: func(int64) (int64, error) { return g, nil }}
			if got, err := s.SeekOffset(target); err != nil || got != want {
				t.Errorf("guess %d target %d: got %d, %v; want %d", g, target, got, err, want)
			}
		}
	}
}
