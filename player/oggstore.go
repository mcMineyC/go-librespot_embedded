package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/vorbis"
)

// storeCoverTimeout bounds the cover download when storing a track. Playback
// waits for the file header, which carries the cover, so keep it short.
const storeCoverTimeout = 5 * time.Second

// OggStore lets the player keep tracks as tagged Ogg Vorbis files and play
// them from there.
//
// When OggPath returns a path for a track about to play:
//   - if the file exists, it is played from disk and nothing is downloaded
//     except metadata;
//   - otherwise the track is downloaded into a temporary file next to path,
//     playback decodes from that file as it fills, and the file is renamed to
//     path once complete. OggStored then reports the outcome.
type OggStore interface {
	// OggPath returns where to keep the track playback requested, or "" to
	// stream it without storing.
	OggPath(requested librespot.SpotifyId) string
	// OggStored is called once per download, on its own goroutine, after the
	// file was moved to path or, when err is set, after the partial file was
	// removed. media may have a different id from requested when Spotify
	// relinked the track.
	OggStored(requested librespot.SpotifyId, media *librespot.Media, path string, err error)
}

// SetOggStore installs s, or removes the store when s is nil. Downloads in
// progress continue either way.
func (p *Player) SetOggStore(s OggStore) {
	p.oggMu.Lock()
	p.oggStore = s
	p.oggMu.Unlock()
}

// storedOgg is a playback input backed by a stored (or downloading) file.
type storedOgg interface {
	librespot.SizedReadAtSeeker
	io.Closer
	seekIndex() vorbis.SeekIndex
}

// openStoredOgg returns the stored file for requested, starting its download
// first when needed. It returns nil, nil when there is no store or the store
// declines the track.
func (p *Player) openStoredOgg(ctx context.Context, log librespot.Logger, client *http.Client, playbackId []byte, requested, spotId librespot.SpotifyId, media *librespot.Media, file *metadatapb.AudioFile) (storedOgg, error) {
	p.oggMu.Lock()
	if p.oggStore == nil {
		p.oggMu.Unlock()
		return nil, nil
	}
	store := p.oggStore
	path := store.OggPath(requested)
	if path == "" {
		p.oggMu.Unlock()
		return nil, nil
	}
	if w := p.oggWrites[path]; w != nil {
		p.oggMu.Unlock()
		log.Infof("playing %s while it downloads", path)
		return w.newReader()
	}
	if _, err := os.Stat(path); err == nil {
		p.oggMu.Unlock()
		log.Infof("playing stored %s", path)
		return openOggFile(path)
	}
	w := newOggWrite(path)
	if p.oggWrites == nil {
		p.oggWrites = map[string]*oggWrite{}
	}
	p.oggWrites[path] = w
	p.oggMu.Unlock()

	// The cover goes into the file header, which playback needs first, so
	// fetch it while the audio key and storage are being resolved.
	cover := make(chan *vorbis.Picture, 1)
	go func() { cover <- fetchCoverLogged(context.Background(), log, client, media, storeCoverTimeout) }()

	src, err := p.openSpotifyOgg(ctx, log, client, playbackId, spotId, file)
	if err == nil {
		err = w.create(src)
	}
	if err != nil {
		p.oggMu.Lock()
		delete(p.oggWrites, path)
		p.oggMu.Unlock()
		w.finish(err)
		return nil, err
	}

	r, err := w.newReader()
	go func() {
		err := w.run(src, media, <-cover)
		p.oggMu.Lock()
		delete(p.oggWrites, path)
		p.oggMu.Unlock()
		if err != nil {
			log.WithError(err).Warnf("failed storing %s", path)
		} else {
			log.Infof("stored %s", path)
		}
		store.OggStored(requested, media, path, err)
	}()
	return r, err
}

// oggWrite is one track being downloaded into the store. Readers block until
// the bytes they need have been written.
type oggWrite struct {
	path string

	mu      sync.Mutex
	cond    *sync.Cond
	tmp     *os.File
	written int64
	done    bool
	err     error

	// srcSize and headerDelta (output header size minus input header size)
	// give the final size and map Spotify seek-table offsets into the file.
	srcSize     int64
	headerKnown bool
	headerDelta int64
	meta        *vorbis.MetadataPage
}

func newOggWrite(path string) *oggWrite {
	w := &oggWrite{path: path}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// create opens the temporary file. Readers wait until it exists.
func (w *oggWrite) create(src *spotifyOgg) error {
	tmp, err := os.CreateTemp(filepath.Dir(w.path), "."+filepath.Base(w.path)+".*.part")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	w.mu.Lock()
	w.tmp, w.srcSize, w.meta = tmp, src.audio.Size(), src.meta
	w.cond.Broadcast()
	w.mu.Unlock()
	return nil
}

// run downloads src into the temporary file and moves it into place.
func (w *oggWrite) run(src *spotifyOgg, media *librespot.Media, pic *vorbis.Picture) error {
	onHeader := func(srcHeader, dstHeader int64) {
		w.mu.Lock()
		w.headerKnown, w.headerDelta = true, dstHeader-srcHeader
		w.cond.Broadcast()
		w.mu.Unlock()
	}
	err := writeTaggedOgg(oggWriteCounter{w}, src.audio, src.audio.Size(), media, src.replayGain(), pic, onHeader)
	if err == nil {
		// CreateTemp makes the file owner-only; give it normal permissions.
		err = w.tmp.Chmod(0o644)
	}
	if err == nil {
		err = w.tmp.Sync()
	}
	if cerr := w.tmp.Close(); err == nil && cerr != nil {
		err = cerr
	}

	w.mu.Lock()
	if err == nil {
		// Renamed under the lock so newReader opens either the temporary
		// file before the rename or the final file after it.
		if rerr := os.Rename(w.tmp.Name(), w.path); rerr != nil {
			err = fmt.Errorf("moving file into place: %w", rerr)
		}
	}
	if err != nil {
		_ = os.Remove(w.tmp.Name())
	}
	w.mu.Unlock()

	w.finish(err)
	return err
}

func (w *oggWrite) finish(err error) {
	w.mu.Lock()
	w.done, w.err = true, err
	w.cond.Broadcast()
	w.mu.Unlock()
}

type oggWriteCounter struct{ w *oggWrite }

func (c oggWriteCounter) Write(b []byte) (int, error) {
	n, err := c.w.tmp.Write(b)
	c.w.mu.Lock()
	c.w.written += int64(n)
	c.w.cond.Broadcast()
	c.w.mu.Unlock()
	return n, err
}

// newReader opens a reader over the file being written, or over the final
// file if the write has already finished.
func (w *oggWrite) newReader() (storedOgg, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.tmp == nil && !w.done {
		w.cond.Wait()
	}
	if w.done {
		if w.err != nil {
			return nil, w.err
		}
		return openOggFile(w.path)
	}
	f, err := os.Open(w.tmp.Name())
	if err != nil {
		return nil, err
	}
	return &growingOgg{w: w, f: f}, nil
}

// growingOgg reads a file that is still being written.
type growingOgg struct {
	w   *oggWrite
	f   *os.File
	pos int64
}

func (g *growingOgg) ReadAt(p []byte, off int64) (int, error) {
	w := g.w
	need := off + int64(len(p))
	w.mu.Lock()
	for !w.done && w.written < need {
		w.cond.Wait()
	}
	err := w.err
	w.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("downloading track: %w", err)
	}
	return g.f.ReadAt(p, off)
}

func (g *growingOgg) Read(p []byte) (int, error) {
	n, err := g.ReadAt(p, g.pos)
	g.pos += int64(n)
	return n, err
}

func (g *growingOgg) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += g.pos
	case io.SeekEnd:
		offset += g.Size()
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	g.pos = offset
	return offset, nil
}

// Size is exact once the write is done. Before that it is the expected size,
// which can be a few bytes too large (reads past the real end return EOF).
func (g *growingOgg) Size() int64 {
	w := g.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return w.written
	}
	return max(w.written, w.srcSize+w.headerDelta)
}

func (g *growingOgg) Close() error { return g.f.Close() }

// seekIndex searches the file by granule position, starting from Spotify's
// seek table so a seek only waits for the bytes around its target.
func (g *growingOgg) seekIndex() vorbis.SeekIndex {
	w := g.w
	return &vorbis.GranuleSeeker{
		R:    g,
		Size: g.Size,
		Guess: func(samples int64) (int64, error) {
			w.mu.Lock()
			for !w.headerKnown && !w.done {
				w.cond.Wait()
			}
			known, delta, err := w.headerKnown, w.headerDelta, w.err
			w.mu.Unlock()
			if err != nil {
				return 0, err
			} else if !known {
				return 0, errors.New("header size unknown")
			}
			return w.meta.GetSeekPosition(samples) + delta, nil
		},
	}
}

// oggFile is a complete stored file.
type oggFile struct {
	*io.SectionReader
	f *os.File
}

func openOggFile(path string) (*oggFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &oggFile{SectionReader: io.NewSectionReader(f, 0, fi.Size()), f: f}, nil
}

func (o *oggFile) Close() error { return o.f.Close() }

func (o *oggFile) seekIndex() vorbis.SeekIndex {
	return &vorbis.GranuleSeeker{R: o, Size: o.Size}
}
