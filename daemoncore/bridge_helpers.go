package daemoncore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"

	"golang.org/x/oauth2"
	spotifyoauth2 "golang.org/x/oauth2/spotify"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/mpris"
	"github.com/devgianlu/go-librespot/session"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// appEvents stores per-App event channels created by NewAppFromConfig.
// We keep them outside of App so we don't need to modify the original App struct.
var appEvents sync.Map // map[*App]chan ApiEvent

// pendingAuthEntry holds the oauth2.Config and verifier for a pending
// interactive OAuth flow. Entries are keyed by the OAuth 'state' value.
type pendingAuthEntry struct {
	oauthConf *oauth2.Config
	verifier  string
}

// pendingAuths maps oauth state -> *pendingAuthEntry
var pendingAuths sync.Map // map[string]*pendingAuthEntry

// ParseConfigJSON unmarshals a JSON configuration into the daemon Config.
// Supports snake_case keys by using koanf to map provider values to struct tags.
// This allows mobile hosts to send JSON with snake_case fields (common in mobile apps).
func ParseConfigJSON(configJSON string) (*Config, error) {
	// Unmarshal into a generic map first so koanf can apply the mapping rules.
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return nil, fmt.Errorf("invalid config json: %w", err)
	}

	// Use koanf to load the map and unmarshal into the Config struct so that
	// struct tags (koanf:\"...\") are honored and snake_case keys map correctly.
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(m, "."), nil); err != nil {
		return nil, fmt.Errorf("failed loading config: %w", err)
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("failed unmarshalling config: %w", err)
	}
	return &cfg, nil
}

// eventingApiServer wraps an ApiServer and forwards emitted ApiEvent values
// into an internal channel for bridge consumers while delegating actual server
// behavior to the wrapped implementation.
type eventingApiServer struct {
	base ApiServer
	ch   chan ApiEvent
}

func (s *eventingApiServer) Emit(ev *ApiEvent) {
	// best-effort, non-blocking forward into channel
	if ev != nil && s.ch != nil {
		select {
		case s.ch <- *ev:
		default:
		}
	}
	// forward to the underlying server implementation as well
	if s.base != nil {
		s.base.Emit(ev)
	}
}

func (s *eventingApiServer) Receive() <-chan ApiRequest {
	if s.base == nil {
		// return a closed channel to avoid blocking callers
		return make(<-chan ApiRequest)
	}
	return s.base.Receive()
}

// NewAppFromConfig constructs an App from a Config and wires an ApiServer that
// forwards runtime events into a buffered channel. The returned App is ready
// for the bridge to consume events via (*App).ServerEvents().
func NewAppFromConfig(cfg *Config) (*App, error) {
	app, err := NewApp(cfg)
	if err != nil {
		return nil, err
	}
	/*
		app.withCredentials(context.Background(), session.BlobCredentials{
			Username: cfg.Credentials.SpotifyToken.Username,
			Blob:     []byte(cfg.Credentials.SpotifyToken.Data),
		})
			fmt.Printf("checking for credentials in config: username=%s, data=%d bytes\n", cfg.Credentials.SpotifyToken.Username, len(cfg.Credentials.SpotifyToken.Data))
			if cfg != nil && (cfg.Credentials.SpotifyToken.Username != "" && cfg.Credentials.SpotifyToken.Data != "") {
				fmt.Printf("credentials found in config, attempting restore...\n")
				app.state.Credentials.Username = cfg.Credentials.SpotifyToken.Username
				app.state.Credentials.Data = []byte(cfg.Credentials.SpotifyToken.Data)
				fmt.Printf("credentials restored successfully from config\n")
				}*/

	// Create base API server (real or stub depending on config)
	var baseServer ApiServer
	if cfg.Server.Enabled {
		baseServer, err = NewApiServer(app.log, cfg.Server.Address, cfg.Server.Port, cfg.Server.AllowOrigin, cfg.Server.CertFile, cfg.Server.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed creating api server: %w", err)
		}
	} else {
		baseServer, _ = NewStubApiServer(app.log)
	}

	// Create event forwarding channel and wrapper server
	eventCh := make(chan ApiEvent, 64)
	evWrapper := &eventingApiServer{base: baseServer, ch: eventCh}
	app.server = evWrapper

	// store the channel for ServerEvents()
	appEvents.Store(app, eventCh)

	// mpris server handling
	if cfg.MprisEnabled {
		m, err := mpris.NewServer(app.log)
		if err != nil {
			// If MPRIS cannot be created, return error (same semantics as original daemon)
			return nil, fmt.Errorf("failed creating mpris server: %w", err)
		}
		app.mpris = m
	} else {
		app.mpris = mpris.DummyServer{}
	}

	return app, nil
}

// ServerEvents returns a read-only channel of ApiEvent associated with the App.
// Consumers (the mobile bridge) can range over this channel to receive typed
// runtime events. The channel is provided by NewAppFromConfig.
func (a *App) ServerEvents() <-chan ApiEvent {
	if chv, ok := appEvents.Load(a); ok {
		if ch, ok := chv.(chan ApiEvent); ok {
			return ch
		}
	}
	// If no channel is registered, return a closed channel to avoid blocking.
	return make(<-chan ApiEvent)
}

// BeginInteractiveAuth starts an interactive authentication flow and returns
// a URL for the host to open in a browser/webview. The returned URL includes a
// randomly generated `state` parameter. The host must capture the redirect and
// call CompleteInteractiveAuth with the resulting callback URL (or raw code).
func (a *App) BeginInteractiveAuth(ctx context.Context) (string, error) {
	// Build oauth2 config, mirroring the scopes used in CLI flow.
	oauthConf := &oauth2.Config{
		ClientID: librespot.ClientIdHex,
		Scopes: []string{
			"app-remote-control",
			"playlist-modify",
			"playlist-modify-private",
			"playlist-modify-public",
			"playlist-read",
			"playlist-read-collaborative",
			"playlist-read-private",
			"streaming",
			"ugc-image-upload",
			"user-follow-modify",
			"user-follow-read",
			"user-library-modify",
			"user-library-read",
			"user-modify",
			"user-modify-playback-state",
			"user-modify-private",
			"user-personalized",
			"user-read-birthdate",
			"user-read-currently-playing",
			"user-read-email",
			"user-read-play-history",
			"user-read-playback-position",
			"user-read-playback-state",
			"user-read-private",
			"user-read-recently-played",
			"user-top-read",
		},
		Endpoint: spotifyoauth2.Endpoint,
	}

	// If a local callback port is configured, use it as redirect URL (consistency with CLI)
	if a != nil && a.cfg != nil && a.cfg.Credentials.Interactive.CallbackPort != 0 {
		oauthConf.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/login", a.cfg.Credentials.Interactive.CallbackPort)
	} else {
		oauthConf.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/login", 8080)
	}

	verifier := oauth2.GenerateVerifier()

	// Generate a random 'state' to correlate Begin/Complete
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return "", fmt.Errorf("failed generating oauth state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	// Compose the auth URL. Include the state so the host can return it back on redirect.
	authURL := oauthConf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))

	// Store pending entry keyed by state
	pendingAuths.Store(state, &pendingAuthEntry{
		oauthConf: oauthConf,
		verifier:  verifier,
	})

	return authURL, nil
}

// CompleteInteractiveAuth consumes the callback URL (or raw auth code) obtained
// by the host from the browser/webview, exchanges it for tokens and initializes
// a session using the received Spotify token. The callbackURL may be a full URL
// (containing ?code=...&state=...) or a raw authorization code. The function
// attempts to correlate the call to a pending BeginInteractiveAuth by the
// 'state' parameter, and falls back to use the sole pending entry if only one
// exists.
func (a *App) CompleteInteractiveAuth(ctx context.Context, callbackURL string) error {
	// Try parsing as URL and extract code/state
	parsed, err := url.Parse(callbackURL)
	var code string
	var state string
	if err == nil {
		// parsed as URL successfully
		code = parsed.Query().Get("code")
		state = parsed.Query().Get("state")
	} else {
		// treat the callback string as a raw code
		code = callbackURL
	}

	if code == "" {
		return fmt.Errorf("no authorization code found in callback")
	}

	var entry *pendingAuthEntry

	if state != "" {
		if val, ok := pendingAuths.Load(state); ok {
			entry = val.(*pendingAuthEntry)
		} else {
			return fmt.Errorf("no pending interactive auth found for state: %s", state)
		}
	} else {
		// No state provided: if exactly one pending entry exists, use it as a fallback.
		var foundKey string
		var foundVal *pendingAuthEntry
		pendingAuths.Range(func(k, v interface{}) bool {
			foundKey = k.(string)
			foundVal = v.(*pendingAuthEntry)
			// stop after first
			return false
		})
		if foundVal == nil {
			return fmt.Errorf("no pending interactive auth entry available")
		}
		entry = foundVal
		state = foundKey
	}

	// Exchange code for token using stored oauth config and verifier.
	token, err := entry.oauthConf.Exchange(ctx, code, oauth2.VerifierOption(entry.verifier))
	if err != nil {
		return fmt.Errorf("failed exchanging oauth2 code: %w", err)
	}

	// Extract username and Web API token payload from OAuth exchange.
	usernameVal := token.Extra("username")
	username, _ := usernameVal.(string)
	scopeVal := token.Extra("scope")
	scope, _ := scopeVal.(string)

	// Seed in-memory Web API credential manager from interactive auth first.
	// This drives the credential callback payload produced by withCredentials.
	a.SetWebAPICredentials(librespot.ClientIdHex, "", token.RefreshToken)
	a.webAPITokenLock.Lock()
	a.webAPIAccessToken = token.AccessToken
	if !token.Expiry.IsZero() {
		a.webAPIAccessTokenExp = token.Expiry
	}

	// Persist Web API credentials in app state so host persistence paths
	// (DumpStateJSON / credential callback payload) capture them.
	a.state.WebAPICredentials.AccessToken = token.AccessToken
	a.state.WebAPICredentials.RefreshToken = token.RefreshToken
	a.state.WebAPICredentials.TokenType = "Bearer"
	a.state.WebAPICredentials.Scope = scope
	a.state.WebAPICredentials.ClientID = librespot.ClientIdHex
	if !token.Expiry.IsZero() {
		a.state.WebAPICredentials.ExpiresAt = token.Expiry.Unix()
	}
	a.webAPITokenLock.Unlock()

	// Initialize session using spotify token credentials (this persists stored
	// session credentials similar to the CLI interactive flow).
	if err := a.withCredentials(ctx, session.SpotifyTokenCredentials{Username: username, Token: token.AccessToken}); err != nil {
		return fmt.Errorf("failed creating session from spotify token: %w", err)
	}

	// Cleanup pending entry
	pendingAuths.Delete(state)

	return nil
}

// OAuthCredentials contains the credentials obtained from OAuth flow
type OAuthCredentials struct {
	Username     string `json:"username"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// CompleteInteractiveAuthAndReturnCredentials is like CompleteInteractiveAuth but
// also returns the credentials that were obtained from the OAuth flow.
// This allows the caller to persist the credentials for later use.
func (a *App) CompleteInteractiveAuthAndReturnCredentials(ctx context.Context, callbackURL string) (*OAuthCredentials, error) {
	// Try parsing as URL and extract code/state
	parsed, err := url.Parse(callbackURL)
	var code string
	var state string
	if err == nil {
		// parsed as URL successfully
		code = parsed.Query().Get("code")
		state = parsed.Query().Get("state")
	} else {
		// treat the callback string as a raw code
		code = callbackURL
	}

	if code == "" {
		return nil, fmt.Errorf("no authorization code found in callback")
	}

	var entry *pendingAuthEntry

	if state != "" {
		if val, ok := pendingAuths.Load(state); ok {
			entry = val.(*pendingAuthEntry)
		} else {
			return nil, fmt.Errorf("no pending interactive auth found for state: %s", state)
		}
	} else {
		// No state provided: if exactly one pending entry exists, use it as a fallback.
		var foundKey string
		var foundVal *pendingAuthEntry
		pendingAuths.Range(func(k, v interface{}) bool {
			foundKey = k.(string)
			foundVal = v.(*pendingAuthEntry)
			// stop after first
			return false
		})
		if foundVal == nil {
			return nil, fmt.Errorf("no pending interactive auth entry available")
		}
		entry = foundVal
		state = foundKey
	}

	// Exchange code for token using stored oauth config and verifier.
	token, err := entry.oauthConf.Exchange(ctx, code, oauth2.VerifierOption(entry.verifier))
	if err != nil {
		return nil, fmt.Errorf("failed exchanging oauth2 code: %w", err)
	}

	// Extract username and Web API token payload from OAuth exchange.
	usernameVal := token.Extra("username")
	username, _ := usernameVal.(string)
	scopeVal := token.Extra("scope")
	scope, _ := scopeVal.(string)

	// Seed in-memory Web API credential manager from interactive auth first.
	// This drives the credential callback payload produced by withCredentials.
	a.SetWebAPICredentials(librespot.ClientIdHex, "", token.RefreshToken)
	a.webAPITokenLock.Lock()
	a.webAPIAccessToken = token.AccessToken
	if !token.Expiry.IsZero() {
		a.webAPIAccessTokenExp = token.Expiry
	}

	// Persist Web API credentials in app state so host persistence paths
	// (DumpStateJSON / credential callback payload) capture them.
	a.state.WebAPICredentials.AccessToken = token.AccessToken
	a.state.WebAPICredentials.RefreshToken = token.RefreshToken
	a.state.WebAPICredentials.TokenType = "Bearer"
	a.state.WebAPICredentials.Scope = scope
	a.state.WebAPICredentials.ClientID = librespot.ClientIdHex
	if !token.Expiry.IsZero() {
		a.state.WebAPICredentials.ExpiresAt = token.Expiry.Unix()
	}
	a.webAPITokenLock.Unlock()

	// Initialize session using spotify token credentials (this persists stored
	// session credentials similar to the CLI interactive flow).
	if err := a.withCredentials(ctx, session.SpotifyTokenCredentials{Username: username, Token: token.AccessToken}); err != nil {
		return nil, fmt.Errorf("failed creating session from spotify token: %w", err)
	}

	// Cleanup pending entry
	pendingAuths.Delete(state)

	out := &OAuthCredentials{
		Username:     username,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		TokenType:    "Bearer",
		Scope:        scope,
	}
	if !token.Expiry.IsZero() {
		out.ExpiresAt = token.Expiry.Unix()
	}
	return out, nil
}
