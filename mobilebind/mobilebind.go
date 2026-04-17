package mobilebind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	// Use the new stable core package extracted from cmd/daemon.
	daemon "github.com/devgianlu/go-librespot/daemoncore"
)

type EventCallback interface {
	// event: e.g. "playing", "paused", "metadata", "active", "inactive", "stopped", etc
	// payloadJson: raw JSON payload from ApiEvent.Data
	OnEvent(event string, payloadJson string)
	// optional error/status channel to app
	OnLog(level string, message string)
}

// CredentialCallback is the interface that hosts (mobile apps, CLI tools, etc.) implement
// to take responsibility for persisting credentials obtained from the library.
//
// When the host implements this callback and registers it via SetCredentialCallback:
// - The library will invoke OnCredentialsPersist whenever credentials are successfully obtained
// - The library will NOT write state.json; credential persistence is entirely your responsibility
// - You (the host) must handle writing credentials to your desired storage (file, keystore, etc.)
//
// The credentialsJson parameter is a JSON-encoded object with fields:
//
//	{"username": "...", "data": [...]}
//
// where "data" is the opaque stored credential data from the session that can be used
// to restore a session via RestoreCredentials.
//
// Returning an error from OnCredentialsPersist does NOT abort authentication; it is logged
// as a warning and the flow continues. The host can retry persistence later if needed.
type CredentialCallback interface {
	// OnCredentialsPersist is called when credentials should be persisted.
	// credentialsJson: JSON-encoded credentials that the host should save
	// Returns error if persistence failed; non-fatal (logged as warning)
	OnCredentialsPersist(credentialsJson string) error
}

type Librespot struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	doneCh chan struct{}

	app    *daemon.App
	player *daemon.AppPlayer

	cb           EventCallback
	credentialCb CredentialCallback
	started      bool
}

// DeviceInfo describes a Spotify Connect device from the cached cluster
// (gomobile export; same fields as daemoncore.DeviceInfo).
type DeviceInfo struct {
	Id             string
	Name           string
	Type           string
	IsActive       bool
	Volume         int
	SupportsVolume bool
}

// QueueInfo is a snapshot of the playback queue (gomobile export).
type QueueInfo struct {
	Current *TrackInfo
	Prev    []TrackInfo
	Next    []TrackInfo
}

func deviceInfoFromDaemon(d daemon.DeviceInfo) DeviceInfo {
	return DeviceInfo{
		Id:             d.Id,
		Name:           d.Name,
		Type:           d.Type,
		IsActive:       d.IsActive,
		Volume:         d.Volume,
		SupportsVolume: d.SupportsVolume,
	}
}

func queueInfoFromDaemon(q *daemon.QueueInfo) *QueueInfo {
	if q == nil {
		return &QueueInfo{Prev: []TrackInfo{}, Next: []TrackInfo{}}
	}
	out := &QueueInfo{
		Prev: make([]TrackInfo, 0, len(q.Prev)),
		Next: make([]TrackInfo, 0, len(q.Next)),
	}
	if q.Current != nil {
		out.Current = &TrackInfo{URI: q.Current.URI, Name: q.Current.Name}
	}
	for _, t := range q.Prev {
		out.Prev = append(out.Prev, TrackInfo{URI: t.URI, Name: t.Name})
	}
	for _, t := range q.Next {
		out.Next = append(out.Next, TrackInfo{URI: t.URI, Name: t.Name})
	}
	return out
}

func NewLibrespot() *Librespot {
	return &Librespot{}
}

// SetCredentialCallback sets the callback that will be invoked when credentials
// are successfully obtained and should be persisted.
func (l *Librespot) SetCredentialCallback(cb CredentialCallback) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.credentialCb = cb

	// If app is already running, wire the callback to it
	if l.app != nil {
		l.app.SetCredentialPersistCallback(func(username, credsJson string) error {
			return cb.OnCredentialsPersist(credsJson)
		})
	}
}

func (l *Librespot) SetEventCallback(cb EventCallback) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cb = cb

	// If app is already running, wire the callback to it
	if l.app != nil {
		l.dispatchLog("info", "event callback registered")
	}
}

// Config as JSON string to avoid gomobile map/complex type pain.
func (l *Librespot) StartWithConfigJSON(configJSON string, cb EventCallback) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.started {
		return fmt.Errorf("already started")
	}
	l.cb = cb

	cfg, err := daemon.ParseConfigJSON(configJSON)
	if err != nil {
		return fmt.Errorf("invalid config json: %w", err)
	}

	// Normalize/validate mobile defaults
	if cfg.AudioBackend == "" {
		cfg.AudioBackend = "oto"
	}
	if cfg.DeviceType == "" {
		cfg.DeviceType = "smartphone"
	}
	// API server not needed for in-process control
	cfg.Server.Enabled = false
	// Zeroconf is typically not available/desired on mobile hosts
	cfg.ZeroconfEnabled = false

	// If device_id not provided, generate a new one (20 bytes hex) to match daemon expectations
	if cfg.DeviceId == "" {
		b := make([]byte, 20)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("failed generating device id: %w", err)
		}
		cfg.DeviceId = hex.EncodeToString(b)
		l.dispatchLog("info", fmt.Sprintf("generated device_id: %s", cfg.DeviceId))
	}

	// Construct App from config
	app, err := daemon.NewAppFromConfig(cfg)
	if err != nil {
		return err
	}

	player, err := app.NewAppPlayer(context.Background())
	if err != nil {
		l.dispatchLog("warn", fmt.Sprintf("player initialization skipped: %v", err))
		player = nil
	}

	l.ctx, l.cancel = context.WithCancel(context.Background())
	l.doneCh = make(chan struct{})
	l.app = app
	l.player = player

	go l.runEventPump()
	go func() {
		defer close(l.doneCh)
		if l.player != nil {
			if err := l.app.StartPlayer(l.ctx, l.player); err != nil {
				l.dispatchLog("warn", fmt.Sprintf("failed starting player: %v", err))
				<-l.ctx.Done()
				return
			}
			<-l.ctx.Done()
		} else {
			<-l.ctx.Done()
		}
	}()

	// Wire credential callback to app if one was set before startup
	if l.credentialCb != nil {
		l.app.SetCredentialPersistCallback(func(username, credsJson string) error {
			return l.credentialCb.OnCredentialsPersist(credsJson)
		})
	}

	l.started = true
	//l.dispatchLog("info", "librespot started")
	return nil
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

// BeginInteractiveAuth starts an OAuth flow and returns the URL for the host
// to open. Call CompleteInteractiveAuth with the redirect URL when done.
func (l *Librespot) BeginInteractiveAuth() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil {
		return "", fmt.Errorf("not started")
	}
	return l.app.BeginInteractiveAuth(context.Background())
}

// CompleteInteractiveAuth consumes the redirect URL (or raw auth code) after
// the user completes the OAuth flow in a browser/Custom Tab.
func (l *Librespot) CompleteInteractiveAuth(callbackURL string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil {
		return fmt.Errorf("not started")
	}
	_, err := l.app.CompleteInteractiveAuthAndReturnCredentials(context.Background(), callbackURL)
	return err
}

// ---------------------------------------------------------------------------
// State persistence
// ---------------------------------------------------------------------------

// DumpStateJSON returns the daemon's current in-memory state as a JSON string.
// The embedding application is responsible for persisting the returned bytes.
func (l *Librespot) DumpStateJSON() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil {
		return "", fmt.Errorf("not started")
	}
	b, err := l.app.DumpStateJSON()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// LoadStateJSON loads persisted state provided by the embedding application.
func (l *Librespot) LoadStateJSON(stateJson string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil {
		return fmt.Errorf("not started")
	}
	return l.app.LoadStateJSON([]byte(stateJson))
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// OnBackground pauses playback when the host app moves to the background.
func (l *Librespot) OnBackground() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return nil
	}
	return l.player.ApiPause()
}

// OnForeground resumes playback when the host app returns to the foreground.
func (l *Librespot) OnForeground() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return nil
	}
	return l.player.ApiPlay()
}

// Stop shuts down the player and all background goroutines.
func (l *Librespot) Stop() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started {
		return nil
	}
	l.cancel()
	<-l.doneCh
	l.started = false
	l.dispatchLog("info", "librespot stopped")
	return nil
}

// ---------------------------------------------------------------------------
// Basic playback controls
// ---------------------------------------------------------------------------

// Play resumes playback.
func (l *Librespot) Play() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiPlay()
}

// Pause pauses playback.
func (l *Librespot) Pause() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiPause()
}

// Next advances to the next track.
func (l *Librespot) Next() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiNext("")
}

// Previous goes back to the previous track (or seeks to the start if past 3s).
func (l *Librespot) Previous() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiPrev()
}

// SkipToNextTrack skips forward to the track with the given Spotify URI within
// the current context. The URI must appear in the next-tracks list.
func (l *Librespot) SkipToNextTrack(uri string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	if uri == "" {
		return fmt.Errorf("uri must not be empty")
	}
	return l.player.ApiNext(uri)
}

// SkipToPrevTrack skips backward to the track with the given Spotify URI.
// The URI must appear in the prev-tracks list.
func (l *Librespot) SkipToPrevTrack(uri string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	if uri == "" {
		return fmt.Errorf("uri must not be empty")
	}
	return l.player.ApiPrevTo(uri)
}

// ---------------------------------------------------------------------------
// Api* entry points (gomobile-friendly; mirror daemoncore AppPlayer)
// ---------------------------------------------------------------------------

// ApiSeek seeks the current track to positionMs (milliseconds from start).
func (l *Librespot) ApiSeek(positionMs int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSeek(positionMs)
}

// ApiSetRepeat sets repeat mode: off | context | track.
func (l *Librespot) ApiSetRepeat(mode string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSetRepeat(mode)
}

// ApiSetShuffle enables or disables shuffle for the current context.
func (l *Librespot) ApiSetShuffle(enabled bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSetShuffle(enabled)
}

// ApiSetVolume sets volume to vol steps (0..VolumeSteps from config).
func (l *Librespot) ApiSetVolume(vol int32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSetVolume(vol)
}

// ApiGetVolume returns the current volume step and configured maximum.
func (l *Librespot) ApiGetVolume() (value uint32, max uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return 0, 0
	}
	return l.player.ApiGetVolume()
}

// ApiPlayUri loads a Spotify context URI; skipToUri and paused as in daemoncore.
func (l *Librespot) ApiPlayUri(uri, skipToUri string, paused bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	if uri == "" {
		return fmt.Errorf("uri must not be empty")
	}
	return l.player.ApiPlayUri(uri, skipToUri, paused)
}

// ApiAddToQueue appends a track or episode URI to the play-ahead queue.
func (l *Librespot) ApiAddToQueue(uri string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	if uri == "" {
		return fmt.Errorf("uri must not be empty")
	}
	return l.player.ApiAddToQueue(uri)
}

// ApiRemoveFromQueue removes the first matching URI from the next queue.
func (l *Librespot) ApiRemoveFromQueue(uri string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	if uri == "" {
		return fmt.Errorf("uri must not be empty")
	}
	return l.player.ApiRemoveFromQueue(uri)
}

// ApiGetQueue returns a snapshot of current, previous, and next tracks.
func (l *Librespot) ApiGetQueue() *QueueInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return &QueueInfo{Prev: []TrackInfo{}, Next: []TrackInfo{}}
	}
	return queueInfoFromDaemon(l.player.ApiGetQueue())
}

// ApiClearQueueCtx clears user-queued items using ctx.
func (l *Librespot) ApiClearQueueCtx(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return
	}
	l.player.ApiClearQueue(ctx)
}

// ApiClearQueue uses a background context.
func (l *Librespot) ApiClearQueue() {
	l.ApiClearQueueCtx(context.Background())
}

// ApiListDevices returns visible Connect devices from the cached cluster.
func (l *Librespot) ApiListDevices() []DeviceInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return nil
	}
	raw := l.player.ApiListDevices()
	out := make([]DeviceInfo, len(raw))
	for i := range raw {
		out[i] = deviceInfoFromDaemon(raw[i])
	}
	return out
}

// ApiGetMetadataForURI resolves metadata for a Spotify URI and returns it as JSON.
// The response shape depends on URI type (track/album/artist/playlist).
func (l *Librespot) ApiGetMetadataForURI(uri string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil || l.player == nil {
		return "", fmt.Errorf("not started")
	}

	meta, err := l.app.GetMetadataForURI(context.Background(), l.player, uri)
	if err != nil {
		return "", err
	}

	b, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("failed to marshal metadata: %w", err)
	}

	return string(b), nil
}

// ApiGetRootPlaylist fetches the authenticated user's root playlist (library root)
// and returns it as JSON.
func (l *Librespot) ApiGetRootPlaylist() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil || l.player == nil {
		return "", fmt.Errorf("not started")
	}

	root, err := l.app.GetRootPlaylist(context.Background(), l.player)
	if err != nil {
		return "", err
	}

	b, err := json.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("failed to marshal root playlist: %w", err)
	}

	return string(b), nil
}

// GetRootPlaylistJSON is a convenience wrapper around ApiGetRootPlaylist.
// It returns "{}" if the bridge is not started or if the request fails.
func (l *Librespot) GetRootPlaylistJSON() string {
	raw, err := l.ApiGetRootPlaylist()
	if err != nil {
		return "{}"
	}
	return raw
}

// RootLibraryItem is a lightweight, host-friendly representation of a rootlist
// entry suitable for selection UIs.
type RootLibraryItem struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ImageURL    string `json:"image_url,omitempty"`
	Kind        string `json:"kind"`
}

func rootLibraryKindFromURI(uri string) string {
	switch {
	case strings.HasPrefix(uri, "spotify:playlist:"):
		return "playlist"
	case strings.HasPrefix(uri, "spotify:folder:"):
		return "folder"
	case strings.HasPrefix(uri, "spotify:show:"):
		return "show"
	case strings.HasPrefix(uri, "spotify:album:"):
		return "album"
	case strings.HasPrefix(uri, "spotify:artist:"):
		return "artist"
	case strings.HasPrefix(uri, "spotify:user:"):
		return "collection"
	default:
		return "unknown"
	}
}

// ApiGetRootLibraryItems fetches authenticated user rootlist and returns compact JSON.
func (l *Librespot) ApiGetRootLibraryItems() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil || l.player == nil {
		return "", fmt.Errorf("not started")
	}

	libraryItems, err := l.app.GetRootLibraryItems(context.Background(), l.player)
	if err != nil {
		return "", err
	}

	items := make([]RootLibraryItem, 0, len(libraryItems))
	for _, it := range libraryItems {
		if it.URI == "" {
			continue
		}

		name := it.Title
		if name == "" {
			name = it.URI
		}

		items = append(items, RootLibraryItem{
			URI:         it.URI,
			Name:        name,
			Description: it.Description,
			ImageURL:    it.ImageURL,
			Kind:        rootLibraryKindFromURI(it.URI),
		})
	}

	b, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("failed to marshal root library items: %w", err)
	}

	return string(b), nil
}

// GetRootLibraryItemsJSON is convenience wrapper around ApiGetRootLibraryItems.
func (l *Librespot) GetRootLibraryItemsJSON() string {
	raw, err := l.ApiGetRootLibraryItems()
	if err != nil {
		return "[]"
	}
	return raw
}

// ApiSearch performs a Spotify search and returns compact JSON results:
//
//	[
//	  {"spotify_id":"...","title":"...","description":"..."},
//	  ...
//	]
func (l *Librespot) ApiSearch(query string, limit int) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil || l.player == nil {
		return "", fmt.Errorf("not started")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query must not be empty")
	}
	return l.app.Search(context.Background(), l.player, query, limit)
}

// Search is a convenience wrapper around ApiSearch.
// It returns "[]" if the bridge is not started or if the request fails.
func (l *Librespot) Search(query string, limit int) string {
	raw, err := l.ApiSearch(query, limit)
	if err != nil {
		return "[]"
	}
	return raw
}

// ApiSearchSuggestions returns compact JSON suggestions for type-ahead UIs:
//
//	[
//	  {"suggestion":"...","spotify_id":"...","description":"..."},
//	  ...
//	]
func (l *Librespot) ApiSearchSuggestions(query string, limit int) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil || l.player == nil {
		return "", fmt.Errorf("not started")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query must not be empty")
	}
	return l.app.SearchSuggestions(context.Background(), l.player, query, limit)
}

// SearchSuggestions is a convenience wrapper around ApiSearchSuggestions.
// It returns "[]" if the bridge is not started or if the request fails.
func (l *Librespot) SearchSuggestions(query string, limit int) string {
	raw, err := l.ApiSearchSuggestions(query, limit)
	if err != nil {
		return "[]"
	}
	return raw
}

// ApiRecentSearches returns compact JSON recent-search entities:
//
//	[
//	  {"title":"...","spotify_id":"...","description":"...","kind":"..."},
//	  ...
//	]
func (l *Librespot) ApiRecentSearches(limit int) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil || l.player == nil {
		return "", fmt.Errorf("not started")
	}
	return l.app.RecentSearches(context.Background(), l.player, limit)
}

// RecentSearches is a convenience wrapper around ApiRecentSearches.
// It returns "[]" if the bridge is not started or if the request fails.
func (l *Librespot) RecentSearches(limit int) string {
	raw, err := l.ApiRecentSearches(limit)
	if err != nil {
		return "[]"
	}
	return raw
}

// ApiTransferPlaybackCtx moves playback to deviceId using ctx.
func (l *Librespot) ApiTransferPlaybackCtx(ctx context.Context, deviceId string, play bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	if deviceId == "" {
		return fmt.Errorf("deviceId must not be empty")
	}
	return l.player.ApiTransferPlayback(ctx, deviceId, play)
}

// ApiTransferPlayback uses a background context.
func (l *Librespot) ApiTransferPlayback(deviceId string, play bool) error {
	return l.ApiTransferPlaybackCtx(context.Background(), deviceId, play)
}

// ---------------------------------------------------------------------------
// Seek, repeat, shuffle, volume, URI, queue (friendly names)
// ---------------------------------------------------------------------------

// Seek is equivalent to ApiSeek.
func (l *Librespot) Seek(positionMs int64) error {
	return l.ApiSeek(positionMs)
}

// SetRepeat sets the repeat mode. mode must be one of:
//   - "off"     — no repeat
//   - "context" — repeat the current playlist/album
//   - "track"   — repeat the current track indefinitely
func (l *Librespot) SetRepeat(mode string) error {
	return l.ApiSetRepeat(mode)
}

// SetShuffle enables or disables shuffle on the current context.
func (l *Librespot) SetShuffle(enabled bool) error {
	return l.ApiSetShuffle(enabled)
}

// SetVolume sets the playback volume. vol is an integer in the range
// 0..VolumeSteps (typically 0..100). Use GetVolumeJSON to read the current
// value and the configured maximum.
func (l *Librespot) SetVolume(vol int32) error {
	return l.ApiSetVolume(vol)
}

// GetVolumeJSON returns the current volume state as a JSON string:
//
//	{"value": 42, "max": 100}
//
// where "value" is the current volume step and "max" is the configured
// VolumeSteps ceiling. Returns an empty JSON object if not started.
func (l *Librespot) GetVolumeJSON() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return "{}"
	}
	value, max := l.player.ApiGetVolume()
	b, _ := json.Marshal(struct {
		Value uint32 `json:"value"`
		Max   uint32 `json:"max"`
	}{Value: value, Max: max})
	return string(b)
}

// PlayUri starts playback of any Spotify URI. Supported URI types:
//   - spotify:track:<id>
//   - spotify:episode:<id>
//   - spotify:album:<id>
//   - spotify:playlist:<id>
//   - spotify:artist:<id>
//   - spotify:show:<id>
//   - spotify:collection (Liked Songs)
//   - spotify:station:<id>
//
// skipToUri, if non-empty, skips within the context to that specific track.
// paused controls whether playback starts paused.
func (l *Librespot) PlayUri(uri string, skipToUri string, paused bool) error {
	return l.ApiPlayUri(uri, skipToUri, paused)
}

// AddToQueue appends a Spotify track or episode URI to the playback queue.
func (l *Librespot) AddToQueue(uri string) error {
	return l.ApiAddToQueue(uri)
}

// RemoveFromQueue removes the first occurrence of uri from the play-ahead
// (next) queue. Returns an error if the URI is not found in the queue.
func (l *Librespot) RemoveFromQueue(uri string) error {
	return l.ApiRemoveFromQueue(uri)
}

// GetQueueJSON returns a JSON snapshot of the current playback queue:
//
//	{
//	  "current": {"uri": "spotify:track:...", "name": "Track Name"},
//	  "prev":    [{"uri": "...", "name": "..."}, ...],
//	  "next":    [{"uri": "...", "name": "..."}, ...]
//	}
//
// "current" is null when nothing is playing. "prev" is oldest-first.
// Returns "{}" if not started.
func (l *Librespot) GetQueueJSON() string {
	l.mu.Lock()
	if l.player == nil {
		l.mu.Unlock()
		return "{}"
	}
	q := queueInfoFromDaemon(l.player.ApiGetQueue())
	l.mu.Unlock()

	type trackJSON struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	}
	type queueJSON struct {
		Current *trackJSON  `json:"current"`
		Prev    []trackJSON `json:"prev"`
		Next    []trackJSON `json:"next"`
	}

	out := queueJSON{
		Prev: []trackJSON{},
		Next: []trackJSON{},
	}
	if q.Current != nil {
		out.Current = &trackJSON{URI: q.Current.URI, Name: q.Current.Name}
	}
	for _, t := range q.Prev {
		out.Prev = append(out.Prev, trackJSON{URI: t.URI, Name: t.Name})
	}
	for _, t := range q.Next {
		out.Next = append(out.Next, trackJSON{URI: t.URI, Name: t.Name})
	}

	b, _ := json.Marshal(out)
	return string(b)
}

// ClearQueue is equivalent to ApiClearQueue.
func (l *Librespot) ClearQueue() {
	l.ApiClearQueue()
}

// ---------------------------------------------------------------------------
// Context tracks (surrounding the current position)
// ---------------------------------------------------------------------------

// TrackInfo holds the URI and name for a track in the context.
// Returned via ContextTracksJSON for gomobile compatibility.
type TrackInfo struct {
	URI  string
	Name string
}

// ContextTracksJSON returns up to n tracks before and after the current
// position as a JSON string:
//
//	{"prev": [...], "next": [...]}
//
// Each entry has "uri" and "name" fields. "prev" is oldest-first.
func (l *Librespot) ContextTracksJSON(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return `{"prev":[],"next":[]}`
	}

	rawPrev, rawNext := l.player.SurroundingTracks(n)

	type trackJSON struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
	}
	type ctxJSON struct {
		Prev []trackJSON `json:"prev"`
		Next []trackJSON `json:"next"`
	}

	out := ctxJSON{
		Prev: make([]trackJSON, 0, len(rawPrev)),
		Next: make([]trackJSON, 0, len(rawNext)),
	}
	for _, t := range rawPrev {
		out.Prev = append(out.Prev, trackJSON{URI: t.URI, Name: t.Name})
	}
	for _, t := range rawNext {
		out.Next = append(out.Next, trackJSON{URI: t.URI, Name: t.Name})
	}

	b, _ := json.Marshal(out)
	return string(b)
}

// ContextTracks returns up to n tracks before and after the current track.
// prev is ordered oldest-first; next is in play order.
//
// Deprecated: prefer ContextTracksJSON which is gomobile-safe. This method
// returns Go slice types and may not bind correctly on all platforms.
func (l *Librespot) ContextTracks(n int) (prev []TrackInfo, next []TrackInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return nil, nil
	}
	rawPrev, rawNext := l.player.SurroundingTracks(n)
	for _, t := range rawPrev {
		prev = append(prev, TrackInfo{URI: t.URI, Name: t.Name})
	}
	for _, t := range rawNext {
		next = append(next, TrackInfo{URI: t.URI, Name: t.Name})
	}
	return prev, next
}

// ---------------------------------------------------------------------------
// Device listing & transfer
// ---------------------------------------------------------------------------

// ListDevicesJSON returns all visible Spotify Connect devices as a JSON array:
//
//	[
//	  {
//	    "id":              "abc123",
//	    "name":            "Living Room Speaker",
//	    "type":            "SPEAKER",
//	    "is_active":       true,
//	    "volume":          72,
//	    "supports_volume": true
//	  },
//	  ...
//	]
//
// The list is derived from the locally cached connect-state cluster and
// requires no network round-trip. Returns "[]" if not started or if no
// cluster data has been received yet (falls back to the local device only).
func (l *Librespot) ListDevicesJSON() string {
	devices := l.ApiListDevices()
	if len(devices) == 0 {
		return "[]"
	}

	type deviceJSON struct {
		Id             string `json:"id"`
		Name           string `json:"name"`
		Type           string `json:"type"`
		IsActive       bool   `json:"is_active"`
		Volume         int    `json:"volume"`
		SupportsVolume bool   `json:"supports_volume"`
	}

	out := make([]deviceJSON, 0, len(devices))
	for _, d := range devices {
		out = append(out, deviceJSON{
			Id:             d.Id,
			Name:           d.Name,
			Type:           d.Type,
			IsActive:       d.IsActive,
			Volume:         d.Volume,
			SupportsVolume: d.SupportsVolume,
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// TransferPlayback transfers active playback to the device with the given ID.
// Use ListDevicesJSON or ApiListDevices to obtain the device ID. If play is true, playback
// starts immediately on the target device; otherwise it transfers paused.
//
// The transfer uses the connect-state transfer endpoint (same mechanism as
// librespot and the Spotify desktop client), not the Web API.
func (l *Librespot) TransferPlayback(deviceId string, play bool) error {
	return l.ApiTransferPlayback(deviceId, play)
}

// ---------------------------------------------------------------------------
// Event pump & internal helpers
// ---------------------------------------------------------------------------

func (l *Librespot) runEventPump() {
	events := l.app.ServerEvents()
	for ev := range events {
		b, _ := json.Marshal(ev.Data)
		l.dispatchState(string(ev.Type), string(b))
	}
}

func (l *Librespot) dispatchState(event, payload string) {
	if l.cb != nil {
		l.cb.OnEvent(event, payload)
	}
}

func (l *Librespot) dispatchLog(level, msg string) {
	if l.cb != nil {
		l.cb.OnLog(level, msg)
	}
}
