package daemoncore

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	librespot "github.com/devgianlu/go-librespot"
)

// OggDumpResult describes one track the automatic dump finished with.
type OggDumpResult struct {
	// URI is the uri playback requested; MediaURI is set when Spotify
	// relinked it to a different track, whose audio and tags were written.
	URI        string   `json:"uri"`
	MediaURI   string   `json:"media_uri,omitempty"`
	Path       string   `json:"path,omitempty"`
	Title      string   `json:"title,omitempty"`
	Artists    []string `json:"artists,omitempty"`
	Album      string   `json:"album,omitempty"`
	DurationMs int32    `json:"duration_ms,omitempty"`
	// Size is the size of the written file in bytes.
	Size int64 `json:"size,omitempty"`

	// Err is set when the track could not be written; Path is then empty.
	Err error `json:"-"`
}

// OggDumpCallback is called once per track, on the goroutine that wrote it.
type OggDumpCallback func(OggDumpResult)

// oggDumper is the player.OggStore of every player the App creates. While
// dir is set, tracks are downloaded into dir and played from there; while it
// is empty, playback streams as usual.
type oggDumper struct {
	mu  sync.Mutex
	dir string
	cb  OggDumpCallback
}

// SetOggDump enables the dump into dir, or disables it when dir is empty.
//
// While enabled, a track about to play (including the next track, preloaded
// near the end of the current one) is played from <dir>/<base62 id>.ogg if
// that file exists, without downloading its audio. Otherwise it is downloaded
// straight into that file, tagged with metadata, ReplayGain and cover art,
// and playback decodes from the file as it fills; cb reports each new file.
// Downloads in progress when the dump is disabled or the folder changes
// finish in their original folder.
func (app *App) SetOggDump(dir string, cb OggDumpCallback) error {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("invalid ogg dump folder: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("creating ogg dump folder: %w", err)
		}
		dir = abs
	}

	d := &app.oggDump
	d.mu.Lock()
	d.dir = dir
	d.cb = cb
	d.mu.Unlock()
	return nil
}

// OggDumpDir returns the folder tracks are dumped to, or "" when disabled.
func (app *App) OggDumpDir() string {
	app.oggDump.mu.Lock()
	defer app.oggDump.mu.Unlock()
	return app.oggDump.dir
}

// OggPath implements player.OggStore. Files are named after the requested
// id, which is the one hosts see in the queue and library, even when Spotify
// relinked it.
func (d *oggDumper) OggPath(requested librespot.SpotifyId) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dir == "" {
		return ""
	}
	return filepath.Join(d.dir, requested.Base62()+".ogg")
}

// OggStored implements player.OggStore.
func (d *oggDumper) OggStored(requested librespot.SpotifyId, media *librespot.Media, path string, err error) {
	res := oggDumpInfo(requested, media)
	if err != nil {
		res.Err = err
	} else {
		res.Path = path
		if fi, err := os.Stat(path); err == nil {
			res.Size = fi.Size()
		}
	}

	d.mu.Lock()
	cb := d.cb
	d.mu.Unlock()
	if cb != nil {
		cb(res)
	}
}

func oggDumpInfo(requested librespot.SpotifyId, media *librespot.Media) OggDumpResult {
	res := OggDumpResult{URI: requested.Uri(), DurationMs: media.Duration()}
	if mediaUri := media.Id().Uri(); mediaUri != res.URI {
		res.MediaURI = mediaUri
	}
	if media.IsTrack() {
		t := media.Track()
		res.Title = t.GetName()
		for _, a := range t.Artist {
			res.Artists = append(res.Artists, a.GetName())
		}
		res.Album = t.GetAlbum().GetName()
	} else {
		e := media.Episode()
		res.Title = e.GetName()
		if s := e.GetShow(); s != nil {
			res.Album = s.GetName()
			if pub := s.GetPublisher(); pub != "" {
				res.Artists = []string{pub}
			}
		}
	}
	return res
}
