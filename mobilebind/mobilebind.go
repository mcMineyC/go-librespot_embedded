package mobilebind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	l.dispatchLog("info", "librespot started")
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
// Seek
// ---------------------------------------------------------------------------

// Seek seeks to positionMs milliseconds from the start of the current track.
func (l *Librespot) Seek(positionMs int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSeek(positionMs)
}

// ---------------------------------------------------------------------------
// Repeat & Shuffle
// ---------------------------------------------------------------------------

// SetRepeat sets the repeat mode. mode must be one of:
//   - "off"     — no repeat
//   - "context" — repeat the current playlist/album
//   - "track"   — repeat the current track indefinitely
func (l *Librespot) SetRepeat(mode string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSetRepeat(mode)
}

// SetShuffle enables or disables shuffle on the current context.
func (l *Librespot) SetShuffle(enabled bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSetShuffle(enabled)
}

// ---------------------------------------------------------------------------
// Volume
// ---------------------------------------------------------------------------

// SetVolume sets the playback volume. vol is an integer in the range
// 0..VolumeSteps (typically 0..100). Use GetVolumeJSON to read the current
// value and the configured maximum.
func (l *Librespot) SetVolume(vol int32) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiSetVolume(vol)
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

// ---------------------------------------------------------------------------
// URI-based playback
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Queue management
// ---------------------------------------------------------------------------

// AddToQueue appends a Spotify track or episode URI to the playback queue.
// The track will play after the current track (and any other queued tracks).
func (l *Librespot) AddToQueue(uri string) error {
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

// RemoveFromQueue removes the first occurrence of uri from the play-ahead
// (next) queue. Returns an error if the URI is not found in the queue.
// The current track and history (prev) tracks are not affected.
func (l *Librespot) RemoveFromQueue(uri string) error {
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
	defer l.mu.Unlock()
	if l.player == nil {
		return "{}"
	}
	q := l.player.ApiGetQueue()

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

// ClearQueue removes all user-queued tracks from the play-ahead list.
// Context tracks (the normal album/playlist sequence) are not affected.
func (l *Librespot) ClearQueue() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return
	}
	l.player.ApiClearQueue(context.Background())
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
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return "[]"
	}

	devices := l.player.ApiListDevices()

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
// Use ListDevicesJSON to obtain the device ID. If play is true, playback
// starts immediately on the target device; otherwise it transfers paused.
//
// The transfer uses the connect-state transfer endpoint (same mechanism as
// librespot and the Spotify desktop client), not the Web API.
func (l *Librespot) TransferPlayback(deviceId string, play bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	if deviceId == "" {
		return fmt.Errorf("deviceId must not be empty")
	}
	return l.player.ApiTransferPlayback(context.Background(), deviceId, play)
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
