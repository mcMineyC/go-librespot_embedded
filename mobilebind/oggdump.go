package mobilebind

import (
	"encoding/json"
	"fmt"

	daemon "github.com/devgianlu/go-librespot/daemoncore"
)

// OggDumpCallback receives one call per track handled by the automatic Ogg
// dump. Calls arrive on a background goroutine (not the main thread), one per
// track, possibly concurrently for different tracks.
type OggDumpCallback interface {
	// OnOggWritten fires once a track has been completely written and moved
	// into place; the file at path is a finished, tagged Ogg Vorbis file.
	// infoJson:
	//
	//	{"uri":"spotify:track:…","path":"/…/<id>.ogg","title":"…",
	//	 "artists":["…"],"album":"…","duration_ms":215000,"size":5242880}
	//
	// uri is the uri playback requested (as in the queue) and names the
	// file. "media_uri" is added when Spotify relinked it to another track;
	// the file then holds that track's audio and its SPOTIFY_URI tag.
	OnOggWritten(uri string, path string, infoJson string)
	// OnOggDumpError fires when a track could not be written. No partial
	// file is left behind.
	OnOggDumpError(uri string, message string)
}

// oggDumpConfig is read from the "ogg_dump" object of the StartWithConfigJSON
// config:
//
//	"ogg_dump": {"enabled": true, "dir": "/data/user/0/<app>/files/ogg"}
type oggDumpConfig struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"`
}

func parseOggDumpConfig(configJSON string) (oggDumpConfig, error) {
	var wrap struct {
		OggDump *oggDumpConfig `json:"ogg_dump"`
	}
	if err := json.Unmarshal([]byte(configJSON), &wrap); err != nil {
		return oggDumpConfig{}, err
	}
	if wrap.OggDump == nil {
		return oggDumpConfig{}, nil
	}
	if wrap.OggDump.Enabled && wrap.OggDump.Dir == "" {
		return oggDumpConfig{}, fmt.Errorf("ogg_dump.enabled requires ogg_dump.dir")
	}
	return *wrap.OggDump, nil
}

// SetOggDumpCallback registers the callback fired for every dumped track. It
// may be called before or after start; nil removes it.
func (l *Librespot) SetOggDumpCallback(cb OggDumpCallback) {
	l.cbMu.Lock()
	l.oggCb = cb
	l.cbMu.Unlock()
}

// EnableOggDump makes playback go through dir. Each track loaded for
// playback is kept as <dir>/<base62 spotify id>.ogg, tagged with metadata,
// ReplayGain and cover art:
//   - if the file exists, the track plays from it and its audio is not
//     downloaded again;
//   - otherwise the whole track is downloaded straight into that file, and
//     playback decodes from the file while it fills. OnOggWritten fires once
//     it is complete.
//
// The folder is created if needed. An empty dir reuses the folder from the
// config or the last EnableOggDump call. It may be called before start, and
// affects tracks loaded after the call. The next track is preloaded shortly
// before the current one ends, so it is saved even if it is then skipped.
func (l *Librespot) EnableOggDump(dir string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dir == "" {
		dir = l.oggDir
	}
	if dir == "" {
		return fmt.Errorf("no ogg dump folder: pass one or set ogg_dump.dir in the config")
	}
	prevDir, prevEnabled := l.oggDir, l.oggEnabled
	l.oggDir, l.oggEnabled = dir, true
	if err := l.applyOggDumpLocked(); err != nil {
		l.oggDir, l.oggEnabled = prevDir, prevEnabled
		return err
	}
	return nil
}

// DisableOggDump makes newly loaded tracks stream from Spotify again, even if
// a stored file exists. Downloads in progress are finished and still
// reported through the callback. The folder is kept, so a
// later EnableOggDump("") resumes into it.
func (l *Librespot) DisableOggDump() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.oggEnabled = false
	_ = l.applyOggDumpLocked()
}

// IsOggDumpEnabled reports whether tracks are currently being dumped.
func (l *Librespot) IsOggDumpEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.oggEnabled
}

// GetOggDumpDir returns the configured dump folder, even while disabled.
func (l *Librespot) GetOggDumpDir() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.oggDir
}

// applyOggDumpLocked pushes the dump settings to the running app. l.mu must
// be held. Before start it does nothing; StartWithConfigJSON applies them.
func (l *Librespot) applyOggDumpLocked() error {
	if l.app == nil {
		return nil
	}
	if !l.oggEnabled {
		return l.app.SetOggDump("", nil)
	}
	return l.app.SetOggDump(l.oggDir, l.dispatchOggDump)
}

func (l *Librespot) dispatchOggDump(res daemon.OggDumpResult) {
	l.cbMu.RLock()
	cb := l.oggCb
	l.cbMu.RUnlock()

	if res.Err != nil {
		l.dispatchLog("warn", fmt.Sprintf("ogg dump failed for %s: %v", res.URI, res.Err))
		if cb != nil {
			cb.OnOggDumpError(res.URI, res.Err.Error())
		}
		return
	}
	if cb != nil {
		info, _ := json.Marshal(res)
		cb.OnOggWritten(res.URI, res.Path, string(info))
	}
}
