//go:build test_unit

package player

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type constReader struct{}

func (constReader) Read(p []float32) (int, error) {
	for i := range p {
		p[i] = 0.5
	}
	return len(p), nil
}

func TestGainReader(t *testing.T) {
	p := &Player{}
	r := &gainReader{src: constReader{}, gain: &p.gain}
	buf := make([]float32, 4)

	_, _ = r.Read(buf)
	require.Equal(t, float32(0.5), buf[0], "default gain is 1")

	p.SetLocalGain(0.5)
	require.Equal(t, float32(0.5), p.LocalGain())
	_, _ = r.Read(buf)
	require.InDelta(t, 0.25, buf[3], 1e-6)

	p.SetLocalGain(0)
	_, _ = r.Read(buf)
	require.Equal(t, float32(0), buf[0], "gain 0 mutes")

	p.SetLocalGain(7)
	require.Equal(t, float32(1), p.LocalGain(), "gain is clamped to 1")
	p.SetLocalGain(-1)
	require.Equal(t, float32(0), p.LocalGain(), "gain is clamped to 0")
}
