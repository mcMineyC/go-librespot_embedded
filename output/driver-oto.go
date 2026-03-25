package output

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/ebitengine/oto/v3"
)

type otoOutput struct {
	reader librespot.Float32Reader
	ctx    *oto.Context
	player *oto.Player

	volume float32
	paused bool
	closed bool

	lock sync.Mutex
	cond *sync.Cond

	externalVolume bool
	volumeUpdate   chan float32
	err            chan error
}

type otoReader struct {
	out  *otoOutput
	fbuf []float32
	bbuf []byte
}

func newOtoOutput(opts *NewOutputOptions) (*otoOutput, error) {
	op := &otoOutput{
		reader:         opts.Reader,
		volume:         opts.InitialVolume,
		externalVolume: opts.ExternalVolume,
		volumeUpdate:   opts.VolumeUpdate,
		err:            make(chan error, 2),
	}
	op.cond = sync.NewCond(&op.lock)

	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   opts.SampleRate,
		ChannelCount: opts.ChannelCount,
		Format:       oto.FormatSignedInt16LE,
	})
	if err != nil {
		return nil, err
	}
	<-ready
	op.ctx = ctx

	r := &otoReader{
		out:  op,
		fbuf: make([]float32, 4096),
		bbuf: make([]byte, 4096*2),
	}

	op.player = ctx.NewPlayer(r)
	op.player.Play()

	return op, nil
}

func (r *otoReader) Read(p []byte) (int, error) {
	o := r.out

	o.lock.Lock()
	for o.paused && !o.closed {
		o.cond.Wait()
	}
	if o.closed {
		o.lock.Unlock()
		return 0, io.EOF
	}

	vol := o.volume
	external := o.externalVolume
	o.lock.Unlock()

	maxSamples := len(p) / 2
	if maxSamples <= 0 {
		return 0, nil
	}
	if maxSamples > len(r.fbuf) {
		maxSamples = len(r.fbuf)
	}

	n, err := o.reader.Read(r.fbuf[:maxSamples])
	if n > 0 {
		volume := vol * vol
		for i := 0; i < n; i++ {
			s := r.fbuf[i]
			if !external {
				s *= volume
			}
			if s > 1 {
				s = 1
			}
			if s < -1 {
				s = -1
			}
			v := int16(s * 32767)
			binary.LittleEndian.PutUint16(r.bbuf[i*2:], uint16(v))
		}
		copied := copy(p, r.bbuf[:n*2])
		return copied, nil
	}

	if err == io.EOF {
		o.lock.Lock()
		o.paused = true
		o.lock.Unlock()
		return 0, nil
	}
	if err != nil {
		select {
		case o.err <- err:
		default:
		}
		return 0, err
	}

	return 0, nil
}

func (o *otoOutput) Pause() error {
	o.lock.Lock()
	defer o.lock.Unlock()

	if o.closed {
		return nil
	}

	o.paused = true
	o.player.Pause()
	o.cond.Signal()
	return nil
}

func (o *otoOutput) Resume() error {
	o.lock.Lock()
	defer o.lock.Unlock()

	if o.closed {
		return nil
	}

	o.paused = false
	o.player.Play()
	o.cond.Signal()
	return nil
}

func (o *otoOutput) Drop() error             { return nil }
func (o *otoOutput) DelayMs() (int64, error) { return 0, nil }

func (o *otoOutput) SetVolume(v float32) {
	if v < 0 || v > 1 {
		panic(fmt.Sprintf("invalid volume: %f", v))
	}

	o.lock.Lock()
	o.volume = v
	o.lock.Unlock()

	sendVolumeUpdate(o.volumeUpdate, v)
}

func (o *otoOutput) Error() <-chan error { return o.err }

func (o *otoOutput) Close() error {
	o.lock.Lock()
	defer o.lock.Unlock()

	if o.closed {
		return nil
	}
	o.closed = true
	o.cond.Signal()

	_ = o.player.Close()
	return nil
}
