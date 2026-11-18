package mobilebind

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Playback: explicit track lists, status, local gain
// ---------------------------------------------------------------------------

// PlayTracks plays a JSON array of track/episode URIs as one librespot
// context, starting at startIndex and positionMs:
//
//	PlayTracks(`["spotify:track:a","spotify:track:b"]`, 1, 30000, false)
//
// The list behaves like a playlist: next/previous, shuffle, repeat and the
// queue work on it, and it ends (or autoplays) after the last track.
func (l *Librespot) PlayTracks(urisJson string, startIndex int, positionMs int64, paused bool) error {
	p, err := l.playerHandle()
	if err != nil {
		return err
	}
	uris, err := parseURIList(urisJson)
	if err != nil {
		return err
	}
	return p.ApiPlayTracks(uris, startIndex, positionMs, paused)
}

// GetStatusJSON returns the playback state:
//
//	{
//	  "active": true, "playing": true, "paused": false, "buffering": false,
//	  "stopped": false, "context_uri": "...", "track_uri": "...", "track_uid": "...",
//	  "position_ms": 81234, "timestamp_ms": 1731700000000, "duration_ms": 215000,
//	  "shuffle": false, "repeat_context": false, "repeat_track": false,
//	  "repeat_mode": "off", "volume": 42, "volume_max": 100, "local_gain": 1
//	}
//
// position_ms is read from the audio pipeline at timestamp_ms; while
// "playing", add the time elapsed since timestamp_ms. Returns "{}" if not
// started.
func (l *Librespot) GetStatusJSON() string {
	p, err := l.playerHandle()
	if err != nil {
		return "{}"
	}
	out, err := marshalJSON(p.ApiGetStatus())
	if err != nil {
		return "{}"
	}
	return out
}

// SetLocalGain scales output on this device only, from 0 (silent) to 1
// (default). Use it for ducking and fades: unlike SetVolume it is not sent to
// Spotify Connect, so other devices do not see a volume change. It applies to
// the next audio buffer (a few milliseconds), so a host can ramp it for fades.
func (l *Librespot) SetLocalGain(gain float32) {
	p, err := l.playerHandle()
	if err != nil {
		return
	}
	p.ApiSetLocalGain(gain)
}

// ---------------------------------------------------------------------------
// Queue editing
// ---------------------------------------------------------------------------

// SetQueueJSON replaces everything that plays after the current track with a
// JSON array of URIs, in that order. Pass the "next" URIs from GetQueueJSON
// after reordering, removing or inserting entries. The current track keeps
// playing; shuffle is turned off because the order is now explicit.
func (l *Librespot) SetQueueJSON(urisJson string) error {
	p, err := l.playerHandle()
	if err != nil {
		return err
	}
	uris, err := parseURIList(urisJson)
	if err != nil {
		return err
	}
	return p.ApiSetUpcoming(uris)
}

// InsertIntoQueue inserts a track or episode into the user queue at index,
// where 0 plays next. Out of range indexes are clamped (a large index appends
// to the user queue, still ahead of the rest of the context).
func (l *Librespot) InsertIntoQueue(uri string, index int) error {
	p, err := l.playerHandle()
	if err != nil {
		return err
	}
	if uri == "" {
		return fmt.Errorf("uri must not be empty")
	}
	return p.ApiInsertIntoQueue(uri, index)
}

// ---------------------------------------------------------------------------
// Batched metadata and details
// ---------------------------------------------------------------------------

// GetTracksMetadataJSON resolves a JSON array of track/episode URIs to flat
// details in batched requests (hundreds of URIs per request), so a whole
// library takes a handful of calls:
//
//	[
//	  {
//	    "uri": "spotify:track:...", "name": "...", "duration_ms": 215000,
//	    "isrc": "USUM71703861", "explicit": false, "track_number": 3,
//	    "disc_number": 1, "popularity": 71, "has_lyrics": true,
//	    "album": {"uri": "...", "name": "...", "image_url": "...",
//	              "thumbnail_url": "...", "release_date": "2017-06-23",
//	              "artists": [{"uri": "...", "name": "..."}]},
//	    "artists": [{"uri": "...", "name": "..."}]
//	  }
//	]
//
// Results follow the input order. Duplicates and URIs that cannot be resolved
// are left out. Episodes set "is_episode" and describe the show in "album".
// Results are cached in memory.
func (l *Librespot) GetTracksMetadataJSON(urisJson string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	uris, err := parseURIList(urisJson)
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	details, err := app.GetTracksDetails(ctx, p, uris)
	if err != nil {
		return "", err
	}
	return marshalJSON(details)
}

// GetAlbumJSON returns an album with its tracks:
//
//	{"uri", "name", "type", "image_url", "thumbnail_url", "release_date",
//	 "year", "artists": [...], "total_tracks", "label", "copyrights": [...],
//	 "tracks": [ <same shape as GetTracksMetadataJSON> ]}
func (l *Librespot) GetAlbumJSON(uri string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	album, err := app.GetAlbumDetails(ctx, p, uri)
	if err != nil {
		return "", err
	}
	return marshalJSON(album)
}

// GetArtistJSON returns an artist with top tracks, discography and related
// artists:
//
//	{"uri", "name", "image_url", "thumbnail_url", "biography",
//	 "top_tracks": [ <track details> ],
//	 "albums": [ <album summary> ], "singles": [...], "compilations": [...],
//	 "appears_on": [...], "related": [{"uri", "name"}]}
//
// An album summary is {"uri", "name", "type", "image_url", "thumbnail_url",
// "release_date", "year", "artists", "total_tracks"}.
func (l *Librespot) GetArtistJSON(uri string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	artist, err := app.GetArtistDetails(ctx, p, uri)
	if err != nil {
		return "", err
	}
	return marshalJSON(artist)
}

// GetLyricsJSON returns lyrics for a track:
//
//	{"uri", "sync_type": "LINE_SYNCED" | "SYLLABLE_SYNCED" | "UNSYNCED",
//	 "language", "provider",
//	 "lines": [{"start_ms": 12340, "end_ms": 0, "words": "..."}]}
//
// "lines" is empty when the track has no lyrics. For UNSYNCED lyrics the
// start times are 0.
func (l *Librespot) GetLyricsJSON(uri string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	lyrics, err := app.GetLyrics(ctx, p, uri)
	if err != nil {
		return "", err
	}
	return marshalJSON(lyrics)
}

// ---------------------------------------------------------------------------
// Library: Liked Songs, saved albums, playlists
// ---------------------------------------------------------------------------

// GetCollectionTracksJSON returns Liked Songs, newest first:
//
//	[{"uri": "spotify:track:...", "added_at": 1700000000}, ...]
//
// added_at is unix seconds. Resolve metadata with GetTracksMetadataJSON.
func (l *Librespot) GetCollectionTracksJSON() (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	items, err := app.GetLikedTracks(ctx, p)
	if err != nil {
		return "", err
	}
	return marshalJSON(nonNil(items))
}

// GetSavedAlbumsJSON returns saved albums with metadata, newest first:
//
//	[{"uri", "name", "type", "image_url", "thumbnail_url", "release_date",
//	  "year", "artists", "total_tracks", "added_at"}, ...]
//
// added_at is unix seconds.
func (l *Librespot) GetSavedAlbumsJSON() (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	albums, err := app.GetSavedAlbums(ctx, p)
	if err != nil {
		return "", err
	}
	return marshalJSON(nonNil(albums))
}

// GetPlaylistJSON returns a playlist and all of its tracks:
//
//	{"uri", "name", "description", "image_url", "owner", "collaborative",
//	 "revision": "<hex>", "length": 120, "unchanged": false,
//	 "tracks": [{"uri", "added_at", "added_by", "item_id"}]}
//
// added_at is unix milliseconds. Pass the revision from the previous sync as
// knownRevision (or "" to always fetch): if the playlist has not changed,
// "unchanged" is true and "tracks" is empty, which costs one small request.
// GetRootLibraryItemsJSON also reports each playlist's revision, so a sync can
// skip unchanged playlists without calling this at all.
func (l *Librespot) GetPlaylistJSON(uri string, knownRevision string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	pl, err := app.GetPlaylist(ctx, p, uri, strings.TrimSpace(knownRevision))
	if err != nil {
		return "", err
	}
	return marshalJSON(pl)
}

// ---------------------------------------------------------------------------
// Library editing
// ---------------------------------------------------------------------------

// Like saves a JSON array of URIs to the library: tracks go to Liked Songs,
// albums to saved albums, artists are followed, shows are followed and
// episodes go to Your Episodes.
func (l *Librespot) Like(urisJson string) error {
	return l.setSaved(urisJson, true)
}

// Unlike removes a JSON array of URIs from the library (see Like).
func (l *Librespot) Unlike(urisJson string) error {
	return l.setSaved(urisJson, false)
}

func (l *Librespot) setSaved(urisJson string, saved bool) error {
	app, p, err := l.handles()
	if err != nil {
		return err
	}
	uris, err := parseURIList(urisJson)
	if err != nil {
		return err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	return app.SetSaved(ctx, p, uris, saved)
}

// IsLikedJSON reports whether each URI in a JSON array is in the library:
//
//	{"spotify:track:a": true, "spotify:album:b": false}
func (l *Librespot) IsLikedJSON(urisJson string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	uris, err := parseURIList(urisJson)
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	saved, err := app.AreSaved(ctx, p, uris)
	if err != nil {
		return "", err
	}
	return marshalJSON(saved)
}

// CreatePlaylist creates a playlist, adds it to the top of the user's library
// and returns its URI.
func (l *Librespot) CreatePlaylist(name string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	return app.CreatePlaylist(ctx, p, name)
}

// RenamePlaylist renames a playlist and returns its new revision.
func (l *Librespot) RenamePlaylist(playlistUri string, name string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name must not be empty")
	}
	ctx, cancel := networkCtx()
	defer cancel()
	return app.RenamePlaylist(ctx, p, playlistUri, name, nil)
}

// SetPlaylistDescription changes a playlist's description and returns its new
// revision.
func (l *Librespot) SetPlaylistDescription(playlistUri string, description string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	return app.RenamePlaylist(ctx, p, playlistUri, "", &description)
}

// AddToPlaylist inserts a JSON array of track/episode URIs at position
// (negative appends) and returns the playlist's new revision.
func (l *Librespot) AddToPlaylist(playlistUri string, urisJson string, position int) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	uris, err := parseURIList(urisJson)
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	return app.AddToPlaylist(ctx, p, playlistUri, uris, position)
}

// RemoveFromPlaylist removes the entry at index (0-based) and returns the
// playlist's new revision. Pass the URI you expect at that index as
// expectedUri to have the call fail instead of removing a different track if
// the playlist changed since you read it; pass "" to skip the check.
// To remove several entries, remove from the highest index down so earlier
// indexes stay valid.
func (l *Librespot) RemoveFromPlaylist(playlistUri string, index int, expectedUri string) (string, error) {
	app, p, err := l.handles()
	if err != nil {
		return "", err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	return app.RemoveFromPlaylist(ctx, p, playlistUri, index, strings.TrimSpace(expectedUri))
}

// DeletePlaylist removes a playlist from the user's library, which is what
// Spotify clients do when you delete a playlist you own.
func (l *Librespot) DeletePlaylist(playlistUri string) error {
	app, p, err := l.handles()
	if err != nil {
		return err
	}
	ctx, cancel := networkCtx()
	defer cancel()
	return app.DeletePlaylist(ctx, p, playlistUri)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
