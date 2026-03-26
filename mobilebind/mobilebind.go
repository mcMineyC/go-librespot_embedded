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

type StateCallback interface {
	// event: e.g. "playing", "paused", "metadata", "active", "inactive", "stopped", etc
	// payloadJson: raw JSON payload from ApiEvent.Data
	OnState(event string, payloadJson string)
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

	cb           StateCallback
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

// Config as JSON string to avoid gomobile map/complex type pain.
func (l *Librespot) StartWithConfigJSON(configJSON string, cb StateCallback) error {
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
		// generate 20 random bytes
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

	// If credentials are provided inside the config JSON under the credentials
	// key (e.g. credentials.spotify_token.username / access_token), try to
	// restore them before creating the AppPlayer. If restore succeeds, no
	// interactive auth is necessary; if it fails we log a warning and continue.
	//
	// The daemon's RestoreStoredCredentials accepts either:
	//  - {"username":"...","access_token":"..."}  (access token form)
	//  - {"username":"...","data": ...}           (stored-credentials form)
	// which makes it convenient to pass a small JSON blob here.

	player, err := app.NewAppPlayer(context.Background())
	if err != nil {
		// Core might not yet expose a player in early shim implementations.
		// Log and continue in app-only mode (event pump remains active).
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
			// Start the player via App.StartPlayer which wires ApiRequest/mpris channels.
			if err := l.app.StartPlayer(l.ctx, l.player); err != nil {
				l.dispatchLog("warn", fmt.Sprintf("failed starting player: %v", err))
				// Even if starting fails, wait for stop signal so Stop() behaves as expected.
				<-l.ctx.Done()
				return
			}
			// Block until canceled so this goroutine remains alive while player runs.
			<-l.ctx.Done()
		} else {
			// No player available; keep the goroutine alive until Stop() is called.
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

// For interactive auth bootstrap.
// Returns auth URL; app opens Custom Tab and later calls CompleteInteractiveAuth(...)
func (l *Librespot) BeginInteractiveAuth() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil {
		return "", fmt.Errorf("not started")
	}
	url, err := l.app.BeginInteractiveAuth(context.Background())
	if err != nil {
		return "", err
	}
	return url, nil
}

// Pass redirected URL from Android intent/deep link.
func (l *Librespot) CompleteInteractiveAuth(callbackURL string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.app == nil {
		return fmt.Errorf("not started")
	}

	// The daemon handles credential persistence via the callback registered in SetCredentialCallback
	_, err := l.app.CompleteInteractiveAuthAndReturnCredentials(context.Background(), callbackURL)
	return err
}

// RestoreCredentials restores a session from persisted OAuth credentials.
//
// The credentialsJson should be the exact JSON string that was previously passed to your
// OnCredentialsPersist callback (i.e., the serialized credentials you saved to disk).
//
// This is typically called on app startup to restore a session without requiring interactive auth.

// DumpStateJSON returns the daemon's current in-memory state as a JSON string.
// The embedding application is responsible for persisting the returned bytes.
// This method exposes the daemoncore DumpStateJSON via the mobile bridge.
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
// The host should read the persisted bytes (from file or secure storage) and
// pass them here. The daemon will populate its in-memory state accordingly.
func (l *Librespot) LoadStateJSON(stateJson string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.app == nil {
		return fmt.Errorf("not started")
	}
	return l.app.LoadStateJSON([]byte(stateJson))
}

// OnBackground is called by the host when the app goes to background.
// It maps to pausing playback on the player so mobile audio usage is minimized.
func (l *Librespot) OnBackground() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		// nothing to do
		return nil
	}
	return l.player.ApiPause()
}

// OnForeground is called by the host when the app returns to foreground.
// It maps to resuming playback (if appropriate).
func (l *Librespot) OnForeground() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return nil
	}
	return l.player.ApiPlay()
}

func (l *Librespot) Play() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiPlay()
}

func (l *Librespot) Pause() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiPause()
}

func (l *Librespot) Next() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiNext("")
}

// SkipToNextTrack skips forward to the track with the given Spotify URI within
// the current context. The URI must appear in NextTracks. Use ContextTracks
// to obtain candidate URIs.
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

// SkipToPrevTrack skips backward to the track with the given Spotify URI within
// the current context. The URI must appear in PrevTracks. Use ContextTracks
// to obtain candidate URIs.
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

// TrackInfo holds the URI and name for a track in the context.
type TrackInfo struct {
	URI  string
	Name string
}

// ContextTracks returns up to n tracks before and after the current track.
// prev is ordered oldest-first; next is in play order.
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

func (l *Librespot) Previous() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.player == nil {
		return fmt.Errorf("not started")
	}
	return l.player.ApiPrev()
}

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

func (l *Librespot) runEventPump() {
	events := l.app.ServerEvents() // <-chan daemon.ApiEvent (extract helper from daemon)
	for ev := range events {
		b, _ := json.Marshal(ev.Data)
		l.dispatchState(string(ev.Type), string(b))
	}
}

func (l *Librespot) dispatchState(event, payload string) {
	if l.cb != nil {
		l.cb.OnState(event, payload)
	}
}

func (l *Librespot) dispatchLog(level, msg string) {
	if l.cb != nil {
		l.cb.OnLog(level, msg)
	}
}
