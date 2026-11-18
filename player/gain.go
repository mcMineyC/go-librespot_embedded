package player

import (
	"math"
	"sync/atomic"

	librespot "github.com/devgianlu/go-librespot"
)

// localGain is a multiplier applied to the decoded samples before they reach
// the output. Unlike the volume it is never reported to Spotify Connect, so a
// host can duck or fade playback without other devices seeing a volume change.
type localGain struct {
	// bits holds math.Float32bits(gain) XOR oneBits, so the zero value means
	// a gain of 1 and a gain of 0 is still representable.
	bits atomic.Uint32
}

var oneBits = math.Float32bits(1)

func (g *localGain) get() float32 {
	return math.Float32frombits(g.bits.Load() ^ oneBits)
}

func (g *localGain) set(v float32) {
	g.bits.Store(math.Float32bits(v) ^ oneBits)
}

type gainReader struct {
	src  librespot.Float32Reader
	gain *localGain
}

func (r *gainReader) Read(p []float32) (int, error) {
	n, err := r.src.Read(p)
	if g := r.gain.get(); g != 1 {
		for i := 0; i < n; i++ {
			p[i] *= g
		}
	}
	return n, err
}

// SetLocalGain sets a gain in the range 0..1 that is applied on this device
// only. It takes effect on the next audio buffer and does not change the
// Spotify Connect volume.
func (p *Player) SetLocalGain(gain float32) {
	if math.IsNaN(float64(gain)) {
		gain = 1
	}
	p.gain.set(max(0, min(gain, 1)))
}

// LocalGain returns the gain set with SetLocalGain.
func (p *Player) LocalGain() float32 {
	return p.gain.get()
}
