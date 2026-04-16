package daemoncore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/mpris"
	"github.com/devgianlu/go-librespot/playplay"

	"github.com/devgianlu/go-librespot/apresolve"
	"github.com/devgianlu/go-librespot/player"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	playlist4pb "github.com/devgianlu/go-librespot/proto/spotify/playlist4"
	yourlibrarypb "github.com/devgianlu/go-librespot/proto/spotify/your_library"
	"github.com/devgianlu/go-librespot/session"
	"github.com/devgianlu/go-librespot/zeroconf"
	"github.com/gofrs/flock"
	log "github.com/sirupsen/logrus"
	"golang.org/x/exp/rand"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/knadh/koanf/v2"
	flag "github.com/spf13/pflag"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
)

var errAlreadyRunning = errors.New("go-librespot is already running")

// CredentialPersistCallback is invoked when credentials are successfully obtained and should be persisted.
// The callback receives the username and serialized credentials data as a JSON string.
//
// When a callback is registered via SetCredentialPersistCallback, the daemon will:
// - Invoke this callback with credentials whenever they are obtained (e.g., after OAuth, zeroconf auth)
// - NOT write credentials to state.json (credential persistence is entirely the callback's responsibility)
//
// If no callback is registered, the daemon will persist credentials to state.json in the config directory.
//
// The credentialsJson parameter is a JSON-encoded object with fields:
//
//	{"username": "...", "data": [...]}
//
// where "data" is the opaque stored credential data from the session.
//
// The callback should return nil on success or an error on failure. Returning an error does NOT abort
// the authentication flow; it is logged as a warning.
type CredentialPersistCallback func(username string, credentialsJson string) error

type App struct {
	log librespot.Logger
	cfg *Config

	client *http.Client

	resolver *apresolve.ApResolver

	deviceId    string
	deviceType  devicespb.DeviceType
	clientToken string
	state       librespot.AppState

	server   ApiServer
	mpris    mpris.Server
	logoutCh chan *AppPlayer

	credentialPersistCb CredentialPersistCallback
}

func parseDeviceType(val string) (devicespb.DeviceType, error) {
	valEnum, ok := devicespb.DeviceType_value[strings.ToUpper(val)]
	if !ok {
		return 0, fmt.Errorf("invalid device type: %s", val)
	}

	return devicespb.DeviceType(valEnum), nil
}

func NewApp(cfg *Config) (app *App, err error) {
	app = &App{cfg: cfg, logoutCh: make(chan *AppPlayer)}

	logger := log.StandardLogger()
	logger.SetFormatter(&log.TextFormatter{
		DisableTimestamp: cfg.LogDisableTimestamp,
	})

	app.log = &LogrusAdapter{log.NewEntry(logger)}
	app.client = &http.Client{Timeout: 30 * time.Second}

	app.deviceType, err = parseDeviceType(cfg.DeviceType)
	if err != nil {
		return nil, err
	}

	app.state.SetLogger(app.log)
	app.state.Credentials.Username = cfg.Credentials.SpotifyToken.Username
	if cfg.Credentials.SpotifyToken.Data != "" {
		decoded, err := base64.StdEncoding.DecodeString(cfg.Credentials.SpotifyToken.Data)
		if err != nil {
			// fallback: treat as raw bytes (backwards compat)
			app.state.Credentials.Data = []byte(cfg.Credentials.SpotifyToken.Data)
		} else {
			app.state.Credentials.Data = decoded
		}
	}

	/* Deprecated in faor of passing in persisted state from the host:
	if err := app.state.Read(cfg.ConfigDir); err != nil {
		return nil, err
	}*/

	app.resolver = apresolve.NewApResolver(app.log, app.client)

	if cfg.DeviceId != "" {
		// Use configured device ID.
		app.deviceId = cfg.DeviceId
	} else if app.state.DeviceId != "" {
		// Use device ID generated in a previous run.
		app.deviceId = app.state.DeviceId
	} else {
		// Generate a new device ID.
		deviceIdBytes := make([]byte, 20)
		_, _ = rand.Read(deviceIdBytes)
		app.deviceId = hex.EncodeToString(deviceIdBytes)
		log.Infof("generated new device id: %s", app.deviceId)

		// Save device ID so we can reuse it next time.
		app.state.DeviceId = app.deviceId

		if err := app.state.Write(); err != nil {
			return nil, err
		}
	}

	if cfg.ClientToken != "" {
		app.clientToken = cfg.ClientToken
	}

	if cfg.FlacEnabled && !playplay.Plugin.IsSupported() {
		// FLAC decryption keys are available only with the PlayPlay DRM implementation.
		// Using PlayPlay might get you banned by Spotify.
		return nil, fmt.Errorf("FLAC playback requires a PlayPlay implementation")
	}

	return app, nil
}

// SetCredentialPersistCallback sets a callback that will be invoked when credentials
// are successfully obtained and should be persisted by the host.
//
// When a callback is set:
// - The daemon will invoke this callback with serialized credentials whenever they are obtained
// - The daemon will NOT write state.json; credential persistence becomes the callback's responsibility
// - The callback should implement custom storage (e.g., write to a file, secure keystore, etc.)
//
// When no callback is set:
// - The daemon will persist credentials to state.json in the config directory
// - This is the default behavior for backwards compatibility
//
// The callback can be set either before or after the app is started.
func (app *App) SetCredentialPersistCallback(cb CredentialPersistCallback) {
	app.credentialPersistCb = cb
}

// LoadStateJSON loads persisted state provided by the embedding application.
// The host application (embedding this library) is responsible for reading
// persisted bytes from disk or secure storage and passing them here.
//
// This method forwards the provided JSON bytes to the internal AppState loader
// and will populate in-memory state accordingly. The daemon will not attempt
// any file I/O for state when using this approach.
func (app *App) LoadStateJSON(data []byte) error {
	if app == nil {
		return fmt.Errorf("app is nil")
	}
	// Ensure logger is set on the state so LoadFromJSON can emit logs if needed.
	app.state.SetLogger(app.log)
	if err := app.state.LoadFromJSON(data); err != nil {
		return fmt.Errorf("failed loading state json: %w", err)
	}
	return nil
}

// DumpStateJSON returns the current in-memory state as JSON bytes. The embedding
// application should call this and persist the returned bytes (for example to a
// file or secure keystore). The daemon will not persist state to disk when a
// host-managed persistence model is used.
func (app *App) DumpStateJSON() ([]byte, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}
	return app.state.DumpToJSON()
}

// GetMetadataForURI resolves Spotify metadata for the given URI by delegating
// to the current session Spclient. Supported URI types mirror
// spclient.GetMetadataForURI (track, album, artist, playlist).
func (app *App) GetMetadataForURI(ctx context.Context, p *AppPlayer, uri string) (any, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	if uri == "" {
		return nil, fmt.Errorf("uri must not be empty")
	}

	return p.sess.Spclient().GetMetadataForURI(ctx, uri)
}

// SearchResultItem is a compact search result payload returned by Search.
type SearchResultItem struct {
	SpotifyID   string `json:"spotify_id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// SearchSuggestion is a compact suggestion payload returned by SearchSuggestions.
type SearchSuggestion struct {
	Suggestion  string `json:"suggestion"`
	SpotifyID   string `json:"spotify_id,omitempty"`
	Description string `json:"description,omitempty"`
}

// RootLibraryItem is a flattened, display-friendly item from the user's root library.
type RootLibraryItem struct {
	URI         string `json:"uri"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
}

type rootLibraryMetadata struct {
	Title       string
	Description string
	ImageFileID []byte
	ImageURL    string
}

func imageURLFromSpotifyImageURI(imageURI string) string {
	imageURI = strings.TrimSpace(imageURI)
	if imageURI == "" {
		return ""
	}

	const prefix = "spotify:image:"
	if strings.HasPrefix(imageURI, prefix) {
		hexID := strings.TrimSpace(strings.TrimPrefix(imageURI, prefix))
		if hexID != "" {
			return "https://i.scdn.co/image/" + hexID
		}
	}

	if strings.HasPrefix(imageURI, "https://") || strings.HasPrefix(imageURI, "http://") {
		return imageURI
	}

	return ""
}

func (app *App) buildRootLibraryItemsFromYourLibrary(resp *yourlibrarypb.YourLibraryResponse) []RootLibraryItem {
	if resp == nil {
		return []RootLibraryItem{}
	}

	out := make([]RootLibraryItem, 0, len(resp.GetEntity())+len(resp.GetPinnedEntity()))
	seen := make(map[string]struct{}, len(resp.GetEntity())+len(resp.GetPinnedEntity()))

	appendEntity := func(e *yourlibrarypb.YourLibraryDecoratedEntity) {
		if e == nil || e.GetEntityInfo() == nil {
			return
		}

		info := e.GetEntityInfo()
		uri := strings.TrimSpace(info.GetUri())
		if uri == "" {
			return
		}

		if _, ok := seen[uri]; ok {
			return
		}
		seen[uri] = struct{}{}

		title := strings.TrimSpace(info.GetName())
		if title == "" {
			title = uri
		}

		description := strings.TrimSpace(info.GetGroupLabel())
		if description == "" {
			if p := e.GetPlaylist(); p != nil {
				description = strings.TrimSpace(p.GetCreatorName())
			}
		}

		out = append(out, RootLibraryItem{
			URI:         uri,
			Title:       title,
			Description: description,
			ImageURL:    imageURLFromSpotifyImageURI(info.GetImageUri()),
		})
	}

	for _, e := range resp.GetEntity() {
		appendEntity(e)
	}
	for _, e := range resp.GetPinnedEntity() {
		appendEntity(e)
	}

	return out
}

func (app *App) getYourLibraryDecorated(ctx context.Context, p *AppPlayer) (*yourlibrarypb.YourLibraryResponse, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}

	hg := p.sess.Mercury()
	if hg == nil {
		return nil, fmt.Errorf("mercury client not initialized")
	}

	req := &yourlibrarypb.YourLibraryRequest{
		Header: &yourlibrarypb.YourLibraryRequestHeader{
			RemainingEntities:       true,
			TotalCount:              true,
			Skip:                    0,
			Length:                  500,
			AllPlaylists:            true,
			SeparatePinnedItems:     true,
			NumLinkTypesInPlaylists: true,
			IncludeEvents:           true,
			IncludePrereleases:      true,
			IncludeAuthors:          true,
		},
		UpdateThrottling: 0,
	}

	body, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling your library request: %w", err)
	}

	candidates := []string{
		"hm://your-library-view/v1/root",
		"hm://your-library-view/v1/",
		"hm://your-library-view/v1",
	}

	var lastErr error
	for _, uri := range candidates {
		respBytes, err := hg.Request(ctx, "POST", uri, nil, body)
		if err != nil {
			lastErr = err
			continue
		}

		var resp yourlibrarypb.YourLibraryResponse
		if err := proto.Unmarshal(respBytes, &resp); err != nil {
			lastErr = fmt.Errorf("failed unmarshalling your library response from %s: %w", uri, err)
			continue
		}

		if resp.GetStatusCode() != 0 && resp.GetStatusCode() != 200 {
			lastErr = fmt.Errorf("your library endpoint %s returned status_code=%d error=%s", uri, resp.GetStatusCode(), resp.GetError())
			continue
		}

		if len(resp.GetEntity()) == 0 && len(resp.GetPinnedEntity()) == 0 {
			lastErr = fmt.Errorf("your library endpoint %s returned no entities", uri)
			continue
		}

		app.log.Infof("library metadata pipeline: your-library endpoint %s returned entities=%d pinned=%d", uri, len(resp.GetEntity()), len(resp.GetPinnedEntity()))
		return &resp, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("all your-library endpoints failed")
	}
	return nil, lastErr
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}

func extractSpotifyIDFromSearchItem(item map[string]any) string {
	if id := strings.TrimSpace(stringFromAny(item["spotify_id"])); id != "" {
		return id
	}

	if id := strings.TrimSpace(stringFromAny(item["id"])); id != "" {
		return id
	}

	uri := strings.TrimSpace(stringFromAny(item["uri"]))
	if uri == "" {
		return ""
	}

	parts := strings.Split(uri, ":")
	if len(parts) == 0 {
		return ""
	}

	// keep full spotify URI when present (best for play command)
	if strings.HasPrefix(uri, "spotify:") {
		return uri
	}

	return parts[len(parts)-1]
}

func extractArtistsSummary(item map[string]any) string {
	rawArtists, ok := item["artists"].([]any)
	if !ok || len(rawArtists) == 0 {
		return ""
	}

	names := make([]string, 0, len(rawArtists))
	for _, raw := range rawArtists {
		artist, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		name := strings.TrimSpace(stringFromAny(artist["name"]))
		if name == "" {
			continue
		}

		names = append(names, name)
	}

	return strings.Join(names, ", ")
}

func extractSectionItems(payload map[string]any, section string) []map[string]any {
	// style 1: Web API-like: payload["tracks"]["items"]
	if sec, ok := payload[section].(map[string]any); ok {
		if rawItems, ok := sec["items"].([]any); ok {
			out := make([]map[string]any, 0, len(rawItems))
			for _, raw := range rawItems {
				m, ok := raw.(map[string]any)
				if ok {
					out = append(out, m)
				}
			}
			return out
		}
	}

	// style 2: searchview-like: payload["results"]["track"]["hits"]
	if results, ok := payload["results"].(map[string]any); ok {
		if sec, ok := results[section].(map[string]any); ok {
			if rawItems, ok := sec["hits"].([]any); ok {
				out := make([]map[string]any, 0, len(rawItems))
				for _, raw := range rawItems {
					m, ok := raw.(map[string]any)
					if ok {
						out = append(out, m)
					}
				}
				return out
			}
		}
	}

	return nil
}

func (app *App) mercurySearch(ctx context.Context, p *AppPlayer, query string, limit int) (map[string]any, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}

	hg := p.sess.Mercury()
	if hg == nil {
		return nil, fmt.Errorf("mercury client not initialized")
	}

	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("query must not be empty")
	}

	if limit <= 0 {
		limit = 10
	} else if limit > 50 {
		limit = 50
	}

	escapedQuery := url.PathEscape(q)
	uri := fmt.Sprintf("hm://searchview/km/v4/search/%s?limit=%d&catalogue=from_token&country=from_token", escapedQuery, limit)

	raw, err := hg.Request(ctx, "GET", uri, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed issuing mercury search request: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("failed decoding mercury search response: %w", err)
	}

	return payload, nil
}

// Search performs Mercury-based search and returns compact JSON results.
func (app *App) Search(ctx context.Context, p *AppPlayer, query string, limit int) (string, error) {
	payload, err := app.mercurySearch(ctx, p, query, limit)
	if err != nil {
		return "", err
	}

	sections := []string{"track", "album", "artist", "playlist", "show", "episode", "tracks", "albums", "artists", "playlists", "shows", "episodes"}
	results := make([]SearchResultItem, 0, limit*3)
	seen := make(map[string]struct{}, limit*3)

	for _, section := range sections {
		for _, item := range extractSectionItems(payload, section) {
			id := extractSpotifyIDFromSearchItem(item)
			if id == "" {
				continue
			}

			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}

			title := strings.TrimSpace(stringFromAny(item["name"]))
			if title == "" {
				title = strings.TrimSpace(stringFromAny(item["title"]))
			}
			if title == "" {
				continue
			}

			description := strings.TrimSpace(stringFromAny(item["description"]))
			if description == "" {
				description = extractArtistsSummary(item)
			}

			results = append(results, SearchResultItem{
				SpotifyID:   id,
				Title:       title,
				Description: description,
			})
		}
	}

	raw, err := json.Marshal(results)
	if err != nil {
		return "", fmt.Errorf("failed marshalling search results: %w", err)
	}

	return string(raw), nil
}

// SearchSuggestions performs lightweight Mercury search and returns compact JSON suggestions.
func (app *App) SearchSuggestions(ctx context.Context, p *AppPlayer, query string, limit int) (string, error) {
	if limit <= 0 {
		limit = 8
	} else if limit > 20 {
		limit = 20
	}

	payload, err := app.mercurySearch(ctx, p, query, limit)
	if err != nil {
		return "", err
	}

	sections := []string{"track", "artist", "album", "playlist", "show", "episode", "tracks", "artists", "albums", "playlists", "shows", "episodes"}
	suggestions := make([]SearchSuggestion, 0, limit)
	seen := make(map[string]struct{}, limit)

outer:
	for _, section := range sections {
		for _, item := range extractSectionItems(payload, section) {
			name := strings.TrimSpace(stringFromAny(item["name"]))
			if name == "" {
				name = strings.TrimSpace(stringFromAny(item["title"]))
			}
			if name == "" {
				continue
			}

			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			description := strings.TrimSpace(stringFromAny(item["description"]))
			if description == "" {
				description = extractArtistsSummary(item)
			}

			suggestions = append(suggestions, SearchSuggestion{
				Suggestion:  name,
				SpotifyID:   extractSpotifyIDFromSearchItem(item),
				Description: description,
			})

			if len(suggestions) >= limit {
				break outer
			}
		}
	}

	raw, err := json.Marshal(suggestions)
	if err != nil {
		return "", fmt.Errorf("failed marshalling search suggestions: %w", err)
	}

	return string(raw), nil
}

// GetRootPlaylist fetches authenticated user's root playlist (library root).
// Tries decorated endpoint first (official librespot behavior), then falls back to Mercury rootlist.
func (app *App) GetRootPlaylist(ctx context.Context, p *AppPlayer) (*playlist4pb.SelectedListContent, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}

	if decorated, err := app.getRootPlaylistDecorated(ctx, p); err == nil {
		return decorated, nil
	} else {
		app.log.Debugf("decorated rootlist unavailable, falling back to mercury: %v", err)
	}

	hg := p.sess.Mercury()
	if hg == nil {
		return nil, fmt.Errorf("mercury client not initialized")
	}

	username := p.sess.Username()
	if username == "" {
		return nil, fmt.Errorf("session username is empty")
	}

	resp, err := hg.Request(ctx, "GET", fmt.Sprintf("hm://playlist/user/%s/rootlist", username), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed requesting root playlist: %w", err)
	}

	var out playlist4pb.SelectedListContent
	if err := proto.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("failed unmarshalling root playlist: %w", err)
	}

	return &out, nil
}

func (app *App) getRootPlaylistDecorated(ctx context.Context, p *AppPlayer) (*playlist4pb.SelectedListContent, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	username := p.sess.Username()
	if username == "" {
		return nil, fmt.Errorf("session username is empty")
	}

	resp, err := p.sess.Spclient().Request(
		ctx,
		"GET",
		fmt.Sprintf("/playlist/v2/user/%s/rootlist", username),
		url.Values{
			"decorate": {"revision,attributes,length,owner,capabilities,status_code"},
			"from":     {"0"},
			"length":   {"500"},
		},
		nil,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed requesting decorated rootlist: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading decorated rootlist response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("decorated rootlist returned %d: %s", resp.StatusCode, string(raw))
	}

	var out playlist4pb.SelectedListContent
	if err := proto.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("failed unmarshalling decorated rootlist: %w", err)
	}
	return &out, nil
}

// GetRootLibraryItems returns flattened root library entries with uri/title/description/image_url.
// Includes timing logs for each stage so slow metadata paths are visible.
func (app *App) GetRootLibraryItems(ctx context.Context, p *AppPlayer) ([]RootLibraryItem, error) {
	totalStart := time.Now()

	rootFetchStart := time.Now()
	root, err := app.GetRootPlaylist(ctx, p)
	if err != nil {
		app.log.Warnf("library metadata pipeline: rootlist path unavailable, fallback to your-library decorated path: %v", err)

		yourLibraryStart := time.Now()
		if ylResp, ylErr := app.getYourLibraryDecorated(ctx, p); ylErr == nil {
			items := app.buildRootLibraryItemsFromYourLibrary(ylResp)
			app.log.Infof("library metadata pipeline: your-library decorated fallback took %s, items=%d", time.Since(yourLibraryStart), len(items))
			return items, nil
		} else {
			return nil, fmt.Errorf("failed fetching root library via rootlist and your-library fallback: rootlist=%w, your-library=%v", err, ylErr)
		}
	}
	app.log.Infof("library metadata pipeline: rootlist fetch took %s", time.Since(rootFetchStart))

	contents := root.GetContents()
	if contents == nil {
		app.log.Infof("library metadata pipeline: rootlist empty, total %s", time.Since(totalStart))
		return []RootLibraryItem{}, nil
	}

	items := contents.GetItems()
	metaItems := contents.GetMetaItems()
	out := make([]RootLibraryItem, 0, len(items))

	inlineStart := time.Now()
	missingURIs := make([]string, 0, len(items))
	seenMissing := make(map[string]struct{}, len(items))

	for i, it := range items {
		if it == nil {
			continue
		}
		uri := it.GetUri()
		if uri == "" {
			continue
		}

		var title, desc string
		if i < len(metaItems) && metaItems[i] != nil && metaItems[i].GetAttributes() != nil {
			attrs := metaItems[i].GetAttributes()
			title = attrs.GetName()
			desc = attrs.GetDescription()
		}

		entry := RootLibraryItem{
			URI:         uri,
			Title:       title,
			Description: desc,
		}

		if i < len(metaItems) && metaItems[i] != nil && metaItems[i].GetAttributes() != nil {
			attrs := metaItems[i].GetAttributes()
			app.applyRootLibraryPictureSizes(&entry, attrs)
			app.applyRootLibraryImage(&entry, p, attrs.GetPicture())
		}

		out = append(out, entry)

		needsBatchMetadata := title == "" && shouldAttemptRootLibraryMetadataFallback(uri)
		if !needsBatchMetadata && strings.HasPrefix(uri, "spotify:playlist:") && entry.ImageURL == "" {
			needsBatchMetadata = true
		}
		if needsBatchMetadata {
			if _, ok := seenMissing[uri]; !ok {
				seenMissing[uri] = struct{}{}
				missingURIs = append(missingURIs, uri)
			}
		}
	}
	app.log.Infof("library metadata pipeline: inline extraction took %s, items=%d, missing=%d", time.Since(inlineStart), len(out), len(missingURIs))

	batchStart := time.Now()
	batchMeta, batchErr := app.getBatchedRootLibraryMetadata(ctx, p, missingURIs)
	if batchErr != nil {
		app.log.Warnf("library metadata pipeline: extended metadata batch failed after %s: %v", time.Since(batchStart), batchErr)
		batchMeta = map[string]rootLibraryMetadata{}
	} else {
		app.log.Infof("library metadata pipeline: extended metadata batch took %s, resolved=%d, requested=%d", time.Since(batchStart), len(batchMeta), len(missingURIs))
	}

	for i := range out {
		if m, ok := batchMeta[out[i].URI]; ok {
			if out[i].Title == "" {
				out[i].Title = m.Title
			}
			if out[i].Description == "" {
				out[i].Description = m.Description
			}
			if out[i].ImageURL == "" && m.ImageURL != "" {
				out[i].ImageURL = m.ImageURL
			}
			app.applyRootLibraryImage(&out[i], p, m.ImageFileID)
		}
	}

	fallbackStart := time.Now()
	fallbackCalls := 0

	unresolvedBeforeFallback := make([]string, 0, len(out))
	unresolvedKindsBefore := map[string]int{}
	for i := range out {
		if out[i].Title != "" || !shouldAttemptRootLibraryMetadataFallback(out[i].URI) {
			continue
		}

		unresolvedBeforeFallback = append(unresolvedBeforeFallback, out[i].URI)
		kind := "unknown"
		switch {
		case strings.HasPrefix(out[i].URI, "spotify:playlist:"):
			kind = "playlist"
		case strings.HasPrefix(out[i].URI, "spotify:folder:"):
			kind = "folder"
		case strings.HasPrefix(out[i].URI, "spotify:show:"):
			kind = "show"
		case strings.HasPrefix(out[i].URI, "spotify:album:"):
			kind = "album"
		case strings.HasPrefix(out[i].URI, "spotify:artist:"):
			kind = "artist"
		case strings.HasPrefix(out[i].URI, "spotify:track:"):
			kind = "track"
		case strings.HasPrefix(out[i].URI, "spotify:episode:"):
			kind = "episode"
		case strings.HasPrefix(out[i].URI, "spotify:user:"):
			kind = "collection"
		}
		unresolvedKindsBefore[kind]++
	}

	if len(unresolvedBeforeFallback) > 0 {
		sample := unresolvedBeforeFallback
		if len(sample) > 15 {
			sample = sample[:15]
		}
		app.log.Warnf("library metadata pipeline: unresolved after batch=%d kinds=%v sample=%v", len(unresolvedBeforeFallback), unresolvedKindsBefore, sample)
	}

	for i := range out {
		if out[i].Title != "" || !shouldAttemptRootLibraryMetadataFallback(out[i].URI) {
			continue
		}

		fallbackCalls++
		meta, err := p.sess.Spclient().GetMetadataForURI(ctx, out[i].URI)
		if err == nil {
			app.applySingleMetadataFallback(&out[i], p, meta)
		}

		if out[i].Title == "" {
			out[i].Title = out[i].URI
		}
	}

	unresolvedAfterFallback := make([]string, 0, len(out))
	unresolvedKindsAfter := map[string]int{}
	for i := range out {
		if out[i].Title != "" || !shouldAttemptRootLibraryMetadataFallback(out[i].URI) {
			continue
		}

		unresolvedAfterFallback = append(unresolvedAfterFallback, out[i].URI)
		kind := "unknown"
		switch {
		case strings.HasPrefix(out[i].URI, "spotify:playlist:"):
			kind = "playlist"
		case strings.HasPrefix(out[i].URI, "spotify:folder:"):
			kind = "folder"
		case strings.HasPrefix(out[i].URI, "spotify:show:"):
			kind = "show"
		case strings.HasPrefix(out[i].URI, "spotify:album:"):
			kind = "album"
		case strings.HasPrefix(out[i].URI, "spotify:artist:"):
			kind = "artist"
		case strings.HasPrefix(out[i].URI, "spotify:track:"):
			kind = "track"
		case strings.HasPrefix(out[i].URI, "spotify:episode:"):
			kind = "episode"
		case strings.HasPrefix(out[i].URI, "spotify:user:"):
			kind = "collection"
		}
		unresolvedKindsAfter[kind]++
	}

	app.log.Infof("library metadata pipeline: per-item fallback took %s, calls=%d", time.Since(fallbackStart), fallbackCalls)
	if len(unresolvedAfterFallback) > 0 {
		sample := unresolvedAfterFallback
		if len(sample) > 15 {
			sample = sample[:15]
		}
		app.log.Warnf("library metadata pipeline: unresolved after fallback=%d kinds=%v sample=%v", len(unresolvedAfterFallback), unresolvedKindsAfter, sample)
	}
	app.log.Infof("library metadata pipeline: total took %s", time.Since(totalStart))

	return out, nil
}

func rootLibraryMetadataQueriesForURI(uri string) []*extmetadatapb.ExtensionQuery {
	switch {
	case strings.HasPrefix(uri, "spotify:track:"):
		return []*extmetadatapb.ExtensionQuery{
			{ExtensionKind: extmetadatapb.ExtensionKind_TRACK_V4},
		}
	case strings.HasPrefix(uri, "spotify:album:"):
		return []*extmetadatapb.ExtensionQuery{
			{ExtensionKind: extmetadatapb.ExtensionKind_ALBUM_V4},
		}
	case strings.HasPrefix(uri, "spotify:artist:"):
		return []*extmetadatapb.ExtensionQuery{
			{ExtensionKind: extmetadatapb.ExtensionKind_ARTIST_V4},
		}
	case strings.HasPrefix(uri, "spotify:show:"):
		return []*extmetadatapb.ExtensionQuery{
			{ExtensionKind: extmetadatapb.ExtensionKind_SHOW_V4},
		}
	case strings.HasPrefix(uri, "spotify:episode:"):
		return []*extmetadatapb.ExtensionQuery{
			{ExtensionKind: extmetadatapb.ExtensionKind_EPISODE_V4},
		}
	case strings.HasPrefix(uri, "spotify:playlist:"):
		return []*extmetadatapb.ExtensionQuery{
			{ExtensionKind: extmetadatapb.ExtensionKind_PLAYLIST_ATTRIBUTES_V2},
			{ExtensionKind: extmetadatapb.ExtensionKind_LIST_ATTRIBUTES_V2},
			{ExtensionKind: extmetadatapb.ExtensionKind_LIST_METADATA},
			{ExtensionKind: extmetadatapb.ExtensionKind_LIST_METADATA_V2},
			{ExtensionKind: extmetadatapb.ExtensionKind_VISUAL_IDENTITY_TRAIT},
		}
	default:
		return nil
	}
}

func shouldAttemptRootLibraryMetadataFallback(uri string) bool {
	switch {
	case strings.HasPrefix(uri, "spotify:track:"),
		strings.HasPrefix(uri, "spotify:album:"),
		strings.HasPrefix(uri, "spotify:artist:"),
		strings.HasPrefix(uri, "spotify:show:"),
		strings.HasPrefix(uri, "spotify:episode:"),
		strings.HasPrefix(uri, "spotify:playlist:"):
		return true
	default:
		return false
	}
}

func (app *App) getBatchedRootLibraryMetadata(ctx context.Context, p *AppPlayer, uris []string) (map[string]rootLibraryMetadata, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	if len(uris) == 0 {
		return map[string]rootLibraryMetadata{}, nil
	}

	req := &extmetadatapb.BatchedEntityRequest{
		EntityRequest: make([]*extmetadatapb.EntityRequest, 0, len(uris)),
	}
	for _, uri := range uris {
		if uri == "" {
			continue
		}

		queries := rootLibraryMetadataQueriesForURI(uri)
		if len(queries) == 0 {
			continue
		}

		req.EntityRequest = append(req.EntityRequest, &extmetadatapb.EntityRequest{
			EntityUri: uri,
			Query:     queries,
		})
	}

	if len(req.EntityRequest) == 0 {
		return map[string]rootLibraryMetadata{}, nil
	}

	resp, err := p.sess.Spclient().ExtendedMetadata(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(map[string]rootLibraryMetadata, len(uris))
	kindCounts := map[string]int{}
	statusCounts := map[int32]int{}
	samples := make([]string, 0, 20)

	for _, arr := range resp.GetExtendedMetadata() {
		if arr == nil {
			kindCounts["<nil-array>"]++
			continue
		}

		kindKey := arr.GetExtensionKind().String()
		kindCounts[kindKey] += len(arr.GetExtensionData())

		for _, extData := range arr.GetExtensionData() {
			if extData == nil {
				statusCounts[-1]++
				if len(samples) < cap(samples) {
					samples = append(samples, fmt.Sprintf("kind=%s uri=<nil> status=-1 type_url=<nil-extData>", kindKey))
				}
				continue
			}

			statusCode := int32(-2)
			if extData.GetHeader() != nil {
				statusCode = extData.GetHeader().GetStatusCode()
			}
			statusCounts[statusCode]++

			typeURL := "<nil-any>"
			if extData.GetExtensionData() != nil {
				typeURL = extData.GetExtensionData().GetTypeUrl()
			}

			if len(samples) < cap(samples) {
				samples = append(samples, fmt.Sprintf("kind=%s uri=%s status=%d type_url=%s", kindKey, extData.GetEntityUri(), statusCode, typeURL))
			}

			if extData.GetExtensionData() == nil {
				continue
			}
			if extData.GetHeader() == nil || extData.GetHeader().GetStatusCode() != 200 {
				continue
			}

			uri := extData.GetEntityUri()
			if uri == "" {
				continue
			}

			cur := out[uri]
			switch arr.GetExtensionKind() {
			case extmetadatapb.ExtensionKind_TRACK_V4:
				var m metadatapb.Track
				if err := extData.GetExtensionData().UnmarshalTo(&m); err == nil {
					if cur.Title == "" {
						cur.Title = m.GetName()
					}
					if cur.ImageFileID == nil && m.GetAlbum() != nil {
						cur.ImageFileID = bestImageFileID(m.GetAlbum().GetCoverGroup(), m.GetAlbum().GetCover())
					}
				}
			case extmetadatapb.ExtensionKind_ALBUM_V4:
				var m metadatapb.Album
				if err := extData.GetExtensionData().UnmarshalTo(&m); err == nil {
					if cur.Title == "" {
						cur.Title = m.GetName()
					}
					if cur.ImageFileID == nil {
						cur.ImageFileID = bestImageFileID(m.GetCoverGroup(), m.GetCover())
					}
				}
			case extmetadatapb.ExtensionKind_ARTIST_V4:
				var m metadatapb.Artist
				if err := extData.GetExtensionData().UnmarshalTo(&m); err == nil {
					if cur.Title == "" {
						cur.Title = m.GetName()
					}
					if cur.ImageFileID == nil {
						cur.ImageFileID = bestImageFileID(m.GetPortraitGroup(), m.GetPortrait())
					}
				}
			case extmetadatapb.ExtensionKind_SHOW_V4:
				var m metadatapb.Show
				if err := extData.GetExtensionData().UnmarshalTo(&m); err == nil {
					if cur.Title == "" {
						cur.Title = m.GetName()
					}
					if cur.Description == "" {
						cur.Description = m.GetDescription()
					}
					if cur.ImageFileID == nil {
						cur.ImageFileID = bestImageFileID(m.GetCoverImage(), nil)
					}
				}
			case extmetadatapb.ExtensionKind_EPISODE_V4:
				var m metadatapb.Episode
				if err := extData.GetExtensionData().UnmarshalTo(&m); err == nil {
					if cur.Title == "" {
						cur.Title = m.GetName()
					}
					if cur.Description == "" {
						cur.Description = m.GetDescription()
					}
					if cur.ImageFileID == nil {
						cur.ImageFileID = bestImageFileID(m.GetCoverImage(), nil)
					}
				}
			case extmetadatapb.ExtensionKind_VISUAL_IDENTITY_TRAIT, extmetadatapb.ExtensionKind_LIST_METADATA_V2:
				if cur.ImageURL == "" {
					cur.ImageURL = extractMosaicURLFromAny(extData.GetExtensionData())
				}
			}

			if cur.Title != "" || cur.Description != "" || len(cur.ImageFileID) > 0 || cur.ImageURL != "" {
				out[uri] = cur
			}
		}
	}

	app.log.Infof("library metadata pipeline: extended metadata diagnostics kinds=%v statuses=%v", kindCounts, statusCounts)
	if len(samples) > 0 {
		app.log.Warnf("library metadata pipeline: extended metadata sample=%v", samples)
	}

	return out, nil
}

func bestImageFileID(group *metadatapb.ImageGroup, direct []*metadatapb.Image) []byte {
	if group != nil {
		images := group.GetImage()
		for i := len(images) - 1; i >= 0; i-- {
			if images[i] == nil {
				continue
			}
			if fid := images[i].GetFileId(); len(fid) > 0 {
				return fid
			}
		}
	}

	for i := len(direct) - 1; i >= 0; i-- {
		if direct[i] == nil {
			continue
		}
		if fid := direct[i].GetFileId(); len(fid) > 0 {
			return fid
		}
	}

	return nil
}

func (app *App) applyRootLibraryPictureSizes(item *RootLibraryItem, attrs *playlist4pb.ListAttributes) {
	if item == nil || attrs == nil || item.ImageURL != "" {
		return
	}

	bestRank := 999
	bestURL := ""

	for _, ps := range attrs.GetPictureSize() {
		if ps == nil {
			continue
		}

		u := strings.TrimSpace(ps.GetUrl())
		if u == "" {
			continue
		}

		rank := 100
		switch strings.ToLower(strings.TrimSpace(ps.GetTargetName())) {
		case "xlarge":
			rank = 0
		case "large":
			rank = 1
		case "default":
			rank = 2
		case "small":
			rank = 3
		}

		if rank < bestRank {
			bestRank = rank
			bestURL = u
		}
	}

	if bestURL != "" {
		item.ImageURL = bestURL
	}
}

func extractMosaicURLFromAny(v *anypb.Any) string {
	if v == nil {
		return ""
	}

	const prefix = "https://mosaic.scdn.co/"
	raw := v.GetValue()
	start := bytes.Index(raw, []byte(prefix))
	if start < 0 {
		return ""
	}

	part := raw[start:]
	end := len(part)
	for i := 0; i < len(part); i++ {
		if part[i] < 0x20 {
			end = i
			break
		}
	}

	url := strings.TrimSpace(string(part[:end]))
	if strings.HasPrefix(url, prefix) {
		return url
	}
	return ""
}

func (app *App) applyRootLibraryImage(item *RootLibraryItem, p *AppPlayer, fileID []byte) {
	if item == nil || p == nil || p.prodInfo == nil || len(fileID) == 0 || item.ImageURL != "" {
		return
	}
	if img := p.prodInfo.ImageUrl(fileID); img != nil {
		item.ImageURL = *img
	}
}

func (app *App) applySingleMetadataFallback(item *RootLibraryItem, p *AppPlayer, meta any) {
	if item == nil || meta == nil {
		return
	}

	switch m := meta.(type) {
	case *metadatapb.Track:
		if item.Title == "" {
			item.Title = m.GetName()
		}
		if m.GetAlbum() != nil {
			app.applyRootLibraryImage(item, p, bestImageFileID(m.GetAlbum().GetCoverGroup(), m.GetAlbum().GetCover()))
		}
	case *metadatapb.Album:
		if item.Title == "" {
			item.Title = m.GetName()
		}
		app.applyRootLibraryImage(item, p, bestImageFileID(m.GetCoverGroup(), m.GetCover()))
	case *metadatapb.Artist:
		if item.Title == "" {
			item.Title = m.GetName()
		}
		app.applyRootLibraryImage(item, p, bestImageFileID(m.GetPortraitGroup(), m.GetPortrait()))
	case *metadatapb.Show:
		if item.Title == "" {
			item.Title = m.GetName()
		}
		if item.Description == "" {
			item.Description = m.GetDescription()
		}
		app.applyRootLibraryImage(item, p, bestImageFileID(m.GetCoverImage(), nil))
	case *metadatapb.Episode:
		if item.Title == "" {
			item.Title = m.GetName()
		}
		if item.Description == "" {
			item.Description = m.GetDescription()
		}
		app.applyRootLibraryImage(item, p, bestImageFileID(m.GetCoverImage(), nil))
	case *playlist4pb.SelectedListContent:
		if m.GetAttributes() != nil {
			if item.Title == "" {
				item.Title = m.GetAttributes().GetName()
			}
			if item.Description == "" {
				item.Description = m.GetAttributes().GetDescription()
			}
			app.applyRootLibraryPictureSizes(item, m.GetAttributes())
			app.applyRootLibraryImage(item, p, m.GetAttributes().GetPicture())
		}
	}
}

// RestoreStoredCredentials restores stored session credentials previously serialized
// by the host or saved in state.json. The function accepts a JSON string which may
// be in one of the following forms:
//
//  1. Access token form (produced by some hosts):
//     {"username":"...","access_token":"..."}
//
//  2. Stored credentials form (the daemon's internal form):
//     {"username":"...","data":"<base64-or-bytes-json>"}  // "data" is JSON-marshalled []byte
//
// The method tries to handle both formats and will initialize a session using the
// provided credentials. It delegates to existing helper paths (SpotifyToken or
// withCredentials with session.StoredCredentials) so behavior is consistent with
// normal interactive auth flows.

func (app *App) newAppPlayer(ctx context.Context, creds any) (_ *AppPlayer, err error) {
	appPlayer := &AppPlayer{
		app:          app,
		stop:         make(chan struct{}, 1),
		logout:       app.logoutCh,
		countryCode:  new(string),
		volumeUpdate: make(chan float32, 1),
	}

	appPlayer.prefetchTimer = time.NewTimer(math.MaxInt64)
	appPlayer.prefetchTimer.Stop()

	fmt.Printf("Creds type: %T", creds)
	if appPlayer.sess, err = session.NewSessionFromOptions(ctx, &session.Options{
		Log:         app.log,
		DeviceType:  app.deviceType,
		DeviceId:    app.deviceId,
		ClientToken: app.clientToken,
		Resolver:    app.resolver,
		Client:      app.client,
		AppState:    &app.state,
		Credentials: creds,
	}); err != nil {
		return nil, err
	}

	appPlayer.initState()

	if appPlayer.player, err = player.NewPlayer(&player.Options{
		Spclient: appPlayer.sess.Spclient(),
		AudioKey: appPlayer.sess.AudioKey(),
		Events:   appPlayer.sess.Events(),
		Log:      app.log,

		FlacEnabled: app.cfg.FlacEnabled,

		NormalisationEnabled:      !app.cfg.NormalisationDisabled,
		NormalisationUseAlbumGain: app.cfg.NormalisationUseAlbumGain,
		NormalisationPregain:      app.cfg.NormalisationPregain,

		CountryCode: appPlayer.countryCode,

		AudioBackend:     app.cfg.AudioBackend,
		AudioDevice:      app.cfg.AudioDevice,
		MixerDevice:      app.cfg.MixerDevice,
		MixerControlName: app.cfg.MixerControlName,

		AudioBufferTime:  app.cfg.AudioBufferTime,
		AudioPeriodCount: app.cfg.AudioPeriodCount,

		ExternalVolume: app.cfg.ExternalVolume,
		VolumeUpdate:   appPlayer.volumeUpdate,

		AudioOutputPipe:       app.cfg.AudioOutputPipe,
		AudioOutputPipeFormat: app.cfg.AudioOutputPipeFormat,
	},
	); err != nil {
		return nil, fmt.Errorf("failed initializing player: %w", err)
	}

	return appPlayer, nil
}

// NewAppPlayer is an exported helper that constructs and returns an AppPlayer
// using persisted credentials if present. This provides a stable exported API
// for consumers (like the mobile bridge) to obtain a player instance. If no
// persisted credentials exist, an explicit error is returned to indicate an
// interactive auth flow is required.
func (app *App) NewAppPlayer(ctx context.Context) (*AppPlayer, error) {
	// Prefer persisted zeroconf credentials if present
	if app != nil && app.cfg != nil {
		if app.state.Credentials.Data != nil && len(app.state.Credentials.Data) > 0 {
			creds := session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			}
			return app.newAppPlayer(ctx, creds)
		}
	}

	// No persisted credentials available: instruct callers to perform interactive auth.
	return nil, fmt.Errorf("no stored credentials available: perform interactive auth or supply credentials")
}

// StartPlayer starts the provided AppPlayer's runtime loops. It wires the App's
// ApiServer and MPRIS receive channels into the player's Run method and starts
// it in a new goroutine. This is exported to allow external callers (e.g.
// mobile bridge) to start player processing once they have an AppPlayer.
func (app *App) StartPlayer(ctx context.Context, p *AppPlayer) error {
	if p == nil {
		return fmt.Errorf("player is nil")
	}

	// Resolve API receive channel (use a closed channel as fallback to avoid blocking)
	var apiRecv <-chan ApiRequest
	if app.server != nil {
		apiRecv = app.server.Receive()
	} else {
		apiRecv = make(<-chan ApiRequest)
	}

	// Resolve MPRIS receive channel (use a closed channel as fallback)
	var mprisRecv <-chan mpris.MediaPlayer2PlayerCommand
	if app.mpris != nil {
		mprisRecv = app.mpris.Receive()
	} else {
		mprisRecv = make(<-chan mpris.MediaPlayer2PlayerCommand)
	}

	// Start player run loop in background.
	go p.Run(ctx, apiRecv, mprisRecv)

	return nil
}

func (app *App) Zeroconf(ctx context.Context) error {
	return app.withAppPlayer(ctx, func(ctx context.Context) (*AppPlayer, error) {
		if app.cfg.Credentials.Zeroconf.PersistCredentials && len(app.state.Credentials.Data) > 0 {
			app.log.WithField("username", librespot.ObfuscateUsername(app.state.Credentials.Username)).
				Infof("loading previously persisted zeroconf credentials")
			return app.newAppPlayer(ctx, session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			})
		}

		return nil, nil
	})
}

func (app *App) SpotifyToken(ctx context.Context, username, token string) error {
	return app.withCredentials(ctx, session.SpotifyTokenCredentials{Username: username, Token: token})
}

func (app *App) Interactive(ctx context.Context, callbackPort int) error {
	return app.withCredentials(ctx, session.InteractiveCredentials{CallbackPort: callbackPort})
}

func (app *App) withCredentials(ctx context.Context, creds any) (err error) {
	return app.withAppPlayer(ctx, func(ctx context.Context) (*AppPlayer, error) {
		if len(app.state.Credentials.Data) > 0 {
			fmt.Printf("Using existing stored credentials for user %s\n", librespot.ObfuscateUsername(app.state.Credentials.Username))
			return app.newAppPlayer(ctx, session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			})
		} else {
			appPlayer, err := app.newAppPlayer(ctx, creds)
			if err != nil {
				return nil, err
			}

			// store credentials outside this context in case we get called again
			app.state.Credentials.Username = appPlayer.sess.Username()
			app.state.Credentials.Data = appPlayer.sess.StoredCredentials()

			// Invoke credential persist callback if provided.
			// If callback is set, the host is responsible for persisting credentials;
			// we do not write state.json in this case.
			if app.credentialPersistCb != nil {
				credsJson, _ := json.Marshal(map[string]interface{}{
					"username": app.state.Credentials.Username,
					"data":     app.state.Credentials.Data,
				})
				if err := app.credentialPersistCb(app.state.Credentials.Username, string(credsJson)); err != nil {
					app.log.WithError(err).Warn("credential persist callback failed")
					// Don't fail the auth flow if the callback fails; host can retry persistence later
				}
			} else {
				// No callback provided: persist to state.json (backwards compatibility)
				if err = app.state.Write(); err != nil {
					return nil, err
				}
			}

			app.log.WithField("username", librespot.ObfuscateUsername(appPlayer.sess.Username())).
				Debugf("stored credentials")
			return appPlayer, nil
		}
	})
}

func (app *App) withAppPlayer(ctx context.Context, appPlayerFunc func(context.Context) (*AppPlayer, error)) (err error) {
	// if zeroconf is disabled, there is not much we need to do
	if !app.cfg.ZeroconfEnabled {
		appPlayer, err := appPlayerFunc(ctx)
		if err != nil {
			return err
		} else if appPlayer == nil {
			panic("zeroconf is disabled and no credentials are present")
		}

		appPlayer.Run(ctx, app.server.Receive(), app.mpris.Receive())
		return nil
	}

	// pre fetch resolver endpoints
	if err := app.resolver.FetchAll(ctx); err != nil {
		return fmt.Errorf("failed getting endpoints from resolver: %w", err)
	}

	// start zeroconf server and dispatch
	z, err := zeroconf.NewZeroconf(app.log, app.cfg.ZeroconfPort, app.cfg.DeviceName, app.deviceId, app.deviceType, app.cfg.ZeroconfInterfacesToAdvertise, app.cfg.ZeroconfBackend == "avahi")
	if err != nil {
		return fmt.Errorf("failed initializing zeroconf: %w", err)
	}

	var apiCh chan ApiRequest

	currentPlayer, err := appPlayerFunc(ctx)
	if err != nil {
		return err
	}

	// set current player to the provided one if we have it
	if currentPlayer != nil {
		app.log.WithField("username", librespot.ObfuscateUsername(currentPlayer.sess.Username())).
			Debugf("initializing zeroconf session")

		apiCh = make(chan ApiRequest)
		go currentPlayer.Run(ctx, apiCh, app.mpris.Receive())

		// let zeroconf know that we already have a user
		z.SetCurrentUser(currentPlayer.sess.Username())
	}

	// forward API requests to proper channel only if a session is present
	go func() {
		for {
			select {
			case req := <-app.server.Receive():
				if currentPlayer == nil {
					req.Reply(nil, ErrNoSession)
					break
				}

				// if we are here the channel must exist
				apiCh <- req
			}
		}
	}()

	// listen for logout events and unset session when that happens
	go func() {
		for {
			select {
			case p := <-app.logoutCh:
				// check that the logout request is for the current player
				if p != currentPlayer {
					continue
				}

				currentPlayer.Close()
				currentPlayer = nil

				// close the channel after setting the current session to nil
				close(apiCh)

				// restore the session if there is one.
				// we will restore the session even if it's for the same user, but it shouldn't be an issue
				newAppPlayer, err := appPlayerFunc(ctx)
				if err != nil {
					log.WithError(err).Errorf("failed restoring session after logout")

					// unset the zeroconf user
					z.SetCurrentUser("")
				} else if newAppPlayer == nil {
					// unset the zeroconf user
					z.SetCurrentUser("")
				} else {
					// first create the channel and then assign the current session
					apiCh = make(chan ApiRequest)
					currentPlayer = newAppPlayer

					go newAppPlayer.Run(ctx, apiCh, app.mpris.Receive())

					// let zeroconf know that we already have a user
					z.SetCurrentUser(newAppPlayer.sess.Username())

					app.log.WithField("username", librespot.ObfuscateUsername(currentPlayer.sess.Username())).
						Debugf("restored session after logout")
				}
			}
		}
	}()

	return z.Serve(func(req zeroconf.NewUserRequest) bool {
		if currentPlayer != nil {
			currentPlayer.Close()
			currentPlayer = nil

			// close the channel after setting the current session to nil
			close(apiCh)

			// no need to unset the zeroconf user here as the new one will overwrite it anyway
		}

		newAppPlayer, err := app.newAppPlayer(ctx, session.BlobCredentials{
			Username: req.Username,
			Blob:     req.AuthBlob,
		})
		if err != nil {
			app.log.WithError(err).WithField("username", librespot.ObfuscateUsername(req.Username)).
				Errorf("failed creating new session from %s", req.DeviceName)
			return false
		}

		// first create the channel and then assign the current session
		apiCh = make(chan ApiRequest)
		currentPlayer = newAppPlayer

		if app.cfg.Credentials.Zeroconf.PersistCredentials {
			app.state.Credentials.Username = newAppPlayer.sess.Username()
			app.state.Credentials.Data = newAppPlayer.sess.StoredCredentials()

			if err := app.state.Write(); err != nil {
				log.WithError(err).Errorf("failed persisting zeroconf credentials")
			}

			app.log.WithField("username", librespot.ObfuscateUsername(newAppPlayer.sess.Username())).
				Debugf("persisted zeroconf credentials")
		}

		go newAppPlayer.Run(ctx, apiCh, app.mpris.Receive())
		return true
	})
}

type Config struct {
	ConfigDir string `koanf:"config_dir"`

	// We need to keep this object around, otherwise it gets GC'd and the
	// finalizer will run, probably closing the lock.
	configLock *flock.Flock

	LogLevel                      log.Level `koanf:"log_level"`
	LogDisableTimestamp           bool      `koanf:"log_disable_timestamp"`
	DeviceId                      string    `koanf:"device_id"`
	DeviceName                    string    `koanf:"device_name"`
	DeviceType                    string    `koanf:"device_type"`
	ClientToken                   string    `koanf:"client_token"`
	AudioBackend                  string    `koanf:"audio_backend"`
	AudioDevice                   string    `koanf:"audio_device"`
	MixerDevice                   string    `koanf:"mixer_device"`
	MixerControlName              string    `koanf:"mixer_control_name"`
	AudioBufferTime               int       `koanf:"audio_buffer_time"`
	AudioPeriodCount              int       `koanf:"audio_period_count"`
	AudioOutputPipe               string    `koanf:"audio_output_pipe"`
	AudioOutputPipeFormat         string    `koanf:"audio_output_pipe_format"`
	Bitrate                       int       `koanf:"bitrate"`
	VolumeSteps                   uint32    `koanf:"volume_steps"`
	InitialVolume                 uint32    `koanf:"initial_volume"`
	IgnoreLastVolume              bool      `koanf:"ignore_last_volume"`
	NormalisationDisabled         bool      `koanf:"normalisation_disabled"`
	NormalisationUseAlbumGain     bool      `koanf:"normalisation_use_album_gain"`
	NormalisationPregain          float32   `koanf:"normalisation_pregain"`
	ExternalVolume                bool      `koanf:"external_volume"`
	ZeroconfEnabled               bool      `koanf:"zeroconf_enabled"`
	ZeroconfPort                  int       `koanf:"zeroconf_port"`
	ZeroconfBackend               string    `koanf:"zeroconf_backend"`
	DisableAutoplay               bool      `koanf:"disable_autoplay"`
	ZeroconfInterfacesToAdvertise []string  `koanf:"zeroconf_interfaces_to_advertise"`
	MprisEnabled                  bool      `koanf:"mpris_enabled"`
	FlacEnabled                   bool      `koanf:"flac_enabled"`
	Server                        struct {
		Enabled     bool   `koanf:"enabled"`
		Address     string `koanf:"address"`
		Port        int    `koanf:"port"`
		AllowOrigin string `koanf:"allow_origin"`
		CertFile    string `koanf:"cert_file"`
		KeyFile     string `koanf:"key_file"`

		ImageSize string `koanf:"image_size"`
	} `koanf:"server"`
	Credentials struct {
		Type        string `koanf:"type"`
		Interactive struct {
			CallbackPort int `koanf:"callback_port"`
		} `koanf:"interactive"`
		SpotifyToken struct {
			Username string `koanf:"username"`
			Data     string `koanf:"data"`
		} `koanf:"spotify_token"`
		Zeroconf struct {
			PersistCredentials bool `koanf:"persist_credentials"`
		} `koanf:"zeroconf"`
	} `koanf:"credentials"`
}

func loadConfig(cfg *Config) error {
	f := flag.NewFlagSet("config", flag.ContinueOnError)
	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		os.Exit(0)
	}
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	defaultConfigDir := filepath.Join(userConfigDir, "go-librespot")
	f.StringVar(&cfg.ConfigDir, "config_dir", defaultConfigDir, "the configuration directory")

	var configOverrides []string
	f.StringArrayVarP(&configOverrides, "conf", "c", nil, "override config values (format: field=value, use field1.field2=value for nested fields)")

	err = f.Parse(os.Args[1:])
	if err != nil {
		return err
	}

	// Make config directory if needed.
	err = os.MkdirAll(cfg.ConfigDir, 0o700)
	if err != nil {
		return fmt.Errorf("failed creating config directory: %w", err)
	}

	// Lock the config directory (to ensure multiple instances won't clobber
	// each others state).
	lockFilePath := filepath.Join(cfg.ConfigDir, "lockfile")
	cfg.configLock = flock.New(lockFilePath)
	if locked, err := cfg.configLock.TryLock(); err != nil {
		return fmt.Errorf("could not lock config directory: %w", err)
	} else if !locked {
		// Lock already taken! Looks like go-librespot is already running.
		return fmt.Errorf("%w (lockfile: %s)", errAlreadyRunning, lockFilePath)
	}

	k := koanf.New(".")

	// load default configuration
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"log_level": log.InfoLevel,

		"device_type": "computer",
		"bitrate":     160,

		"audio_backend":            "alsa",
		"audio_device":             "default",
		"audio_output_pipe_format": "s16le",
		"mixer_control_name":       "Master",

		"volume_steps":   100,
		"initial_volume": 100,

		"credentials.type": "zeroconf",

		"zeroconf_backend": "builtin",

		"server.address":    "localhost",
		"server.image_size": "default",
	}, "."), nil)

	// load file configuration (if available)
	var configPath string
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "config.yaml")); os.IsNotExist(err) {
		configPath = filepath.Join(cfg.ConfigDir, "config.yml")
	} else {
		configPath = filepath.Join(cfg.ConfigDir, "config.yaml")
	}

	if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed reading configuration file: %w", err)
		}
	}

	// load command line configuration
	if err := k.Load(posflag.Provider(f, ".", k), nil); err != nil {
		return fmt.Errorf("failed loading command line configuration: %w", err)
	}

	// apply command line config overrides (-c/--conf flags)
	if len(configOverrides) > 0 {
		overrideMap := make(map[string]interface{})
		for _, override := range configOverrides {
			parts := strings.SplitN(override, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid config override format: %s (expected field=value)", override)
			}
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key == "" {
				return fmt.Errorf("invalid config override: empty field name in %s", override)
			}
			overrideMap[key] = value
		}
		if err := k.Load(confmap.Provider(overrideMap, "."), nil); err != nil {
			return fmt.Errorf("failed loading config overrides: %w", err)
		}
	}

	// unmarshal configuration
	if err := k.Unmarshal("", &cfg); err != nil {
		return fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	if cfg.DeviceName == "" {
		cfg.DeviceName = "go-librespot"

		hostname, _ := os.Hostname()
		if hostname != "" {
			cfg.DeviceName += " " + hostname
		}
	}

	return nil
}

func main() {
	rand.Seed(uint64(time.Now().UnixNano()))

	var cfg Config
	if err := loadConfig(&cfg); err != nil {
		if errors.Is(err, errAlreadyRunning) {
			// Print a nice error message instead of a harder-to-read log message.
			_, _ = fmt.Fprintln(os.Stderr, "could not start:", err)
			os.Exit(1)
		}
		log.WithError(err).Fatal("failed loading config")
	}

	// set log level
	log.SetLevel(cfg.LogLevel)

	log.Infof("running go-librespot %s", librespot.VersionNumberString())

	// create new app
	app, err := NewApp(&cfg)
	if err != nil {
		log.WithError(err).Fatal("failed creating app")
	}

	// create api server if needed
	if cfg.Server.Enabled {
		app.server, err = NewApiServer(app.log, cfg.Server.Address, cfg.Server.Port, cfg.Server.AllowOrigin, cfg.Server.CertFile, cfg.Server.KeyFile)
		if err != nil {
			log.WithError(err).Fatal("failed creating api server")
		}
	} else {
		app.server, _ = NewStubApiServer(app.log)
	}

	// create mpris server if needed
	if cfg.MprisEnabled {
		app.mpris, err = mpris.NewServer(app.log)
		if err != nil {
			log.WithError(err).Fatal("failed creating mpris server")
		}
	} else {
		app.mpris = mpris.DummyServer{}
	}

	ctx := context.TODO()

	switch cfg.Credentials.Type {
	case "zeroconf":
		// ensure zeroconf is enabled
		app.cfg.ZeroconfEnabled = true
		if err := app.Zeroconf(ctx); err != nil {
			log.WithError(err).Fatal("failed running zeroconf")
		}
	case "interactive":
		if err := app.Interactive(ctx, cfg.Credentials.Interactive.CallbackPort); err != nil {
			log.WithError(err).Fatal("failed running with interactive auth")
		}
	case "spotify_token":
		if err := app.SpotifyToken(ctx, cfg.Credentials.SpotifyToken.Username, cfg.Credentials.SpotifyToken.Data); err != nil {
			log.WithError(err).Fatal("failed running with username and spotify token")
		}
	default:
		log.Fatalf("unknown credentials: %s", cfg.Credentials.Type)
	}
}
