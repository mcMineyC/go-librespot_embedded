package daemoncore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	librespot "github.com/devgianlu/go-librespot"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"google.golang.org/protobuf/proto"
)

// Flat, JSON-friendly catalog types returned to bridge hosts. They carry only
// what a client needs to display and match items, with image URLs resolved.

type ArtistRef struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type AlbumRef struct {
	URI          string      `json:"uri"`
	Name         string      `json:"name"`
	ImageURL     string      `json:"image_url,omitempty"`
	ThumbnailURL string      `json:"thumbnail_url,omitempty"`
	ReleaseDate  string      `json:"release_date,omitempty"`
	Artists      []ArtistRef `json:"artists,omitempty"`
}

type TrackDetails struct {
	URI         string      `json:"uri"`
	Name        string      `json:"name"`
	DurationMs  int64       `json:"duration_ms"`
	ISRC        string      `json:"isrc,omitempty"`
	Explicit    bool        `json:"explicit"`
	TrackNumber int         `json:"track_number,omitempty"`
	DiscNumber  int         `json:"disc_number,omitempty"`
	Popularity  int         `json:"popularity,omitempty"`
	HasLyrics   bool        `json:"has_lyrics"`
	Album       *AlbumRef   `json:"album,omitempty"`
	Artists     []ArtistRef `json:"artists"`
	// IsEpisode is true for podcast episodes; Album then describes the show.
	IsEpisode bool `json:"is_episode,omitempty"`
}

type AlbumSummary struct {
	URI          string      `json:"uri"`
	Name         string      `json:"name"`
	Type         string      `json:"type,omitempty"`
	ImageURL     string      `json:"image_url,omitempty"`
	ThumbnailURL string      `json:"thumbnail_url,omitempty"`
	ReleaseDate  string      `json:"release_date,omitempty"`
	Year         int         `json:"year,omitempty"`
	Artists      []ArtistRef `json:"artists,omitempty"`
	TotalTracks  int         `json:"total_tracks,omitempty"`
}

type AlbumDetails struct {
	AlbumSummary
	Label      string         `json:"label,omitempty"`
	Copyrights []string       `json:"copyrights,omitempty"`
	Tracks     []TrackDetails `json:"tracks"`
}

type ArtistDetails struct {
	URI          string         `json:"uri"`
	Name         string         `json:"name"`
	ImageURL     string         `json:"image_url,omitempty"`
	ThumbnailURL string         `json:"thumbnail_url,omitempty"`
	Biography    string         `json:"biography,omitempty"`
	TopTracks    []TrackDetails `json:"top_tracks"`
	Albums       []AlbumSummary `json:"albums"`
	Singles      []AlbumSummary `json:"singles"`
	Compilations []AlbumSummary `json:"compilations"`
	AppearsOn    []AlbumSummary `json:"appears_on"`
	Related      []ArtistRef    `json:"related"`
}

type LyricsLine struct {
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms,omitempty"`
	Words   string `json:"words"`
}

type Lyrics struct {
	URI      string `json:"uri"`
	SyncType string `json:"sync_type"` // LINE_SYNCED, SYLLABLE_SYNCED or UNSYNCED
	Language string `json:"language,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Lines is empty when the track has no lyrics.
	Lines []LyricsLine `json:"lines"`
}

// ---------------------------------------------------------------------------
// Image helpers
// ---------------------------------------------------------------------------

func (p *AppPlayer) imageURLForFileID(fileID []byte) string {
	if len(fileID) == 0 {
		return ""
	}
	if p != nil && p.prodInfo != nil {
		if u := p.prodInfo.ImageUrl(fileID); u != nil {
			return *u
		}
	}
	return "https://i.scdn.co/image/" + strings.ToLower(hex.EncodeToString(fileID))
}

// imagePair returns a large (~640px) and a small (~64px) image URL.
func (p *AppPlayer) imagePair(group *metadatapb.ImageGroup, direct []*metadatapb.Image) (large, small string) {
	images := direct
	if group != nil && len(group.GetImage()) > 0 {
		images = group.GetImage()
	}
	if len(images) == 0 {
		return "", ""
	}
	return p.imageURLForFileID(getBestImageIdForSize(images, "large")),
		p.imageURLForFileID(getBestImageIdForSize(images, "small"))
}

const (
	idTypeAlbum  librespot.SpotifyIdType = "album"
	idTypeArtist librespot.SpotifyIdType = "artist"
	idTypeShow   librespot.SpotifyIdType = "show"
)

func uriFromGid(typ librespot.SpotifyIdType, gid []byte) string {
	if len(gid) != 16 {
		return ""
	}
	return librespot.SpotifyIdFromGid(typ, gid).Uri()
}

func formatDate(d *metadatapb.Date) string {
	if d == nil || d.GetYear() == 0 {
		return ""
	}
	switch {
	case d.GetMonth() == 0:
		return fmt.Sprintf("%04d", d.GetYear())
	case d.GetDay() == 0:
		return fmt.Sprintf("%04d-%02d", d.GetYear(), d.GetMonth())
	default:
		return fmt.Sprintf("%04d-%02d-%02d", d.GetYear(), d.GetMonth(), d.GetDay())
	}
}

func artistRefs(artists []*metadatapb.Artist) []ArtistRef {
	out := make([]ArtistRef, 0, len(artists))
	for _, a := range artists {
		if a == nil {
			continue
		}
		out = append(out, ArtistRef{URI: uriFromGid(idTypeArtist, a.GetGid()), Name: a.GetName()})
	}
	return out
}

// ---------------------------------------------------------------------------
// Proto -> flat conversion
// ---------------------------------------------------------------------------

func (p *AppPlayer) trackDetailsFromProto(uri string, t *metadatapb.Track) TrackDetails {
	if uri == "" {
		uri = uriFromGid(librespot.SpotifyIdTypeTrack, t.GetGid())
	}
	out := TrackDetails{
		URI:         uri,
		Name:        t.GetName(),
		DurationMs:  int64(t.GetDuration()),
		Explicit:    t.GetExplicit(),
		TrackNumber: int(t.GetNumber()),
		DiscNumber:  int(t.GetDiscNumber()),
		Popularity:  int(t.GetPopularity()),
		HasLyrics:   t.GetHasLyrics(),
		Artists:     artistRefs(t.GetArtist()),
	}
	for _, id := range t.GetExternalId() {
		if strings.EqualFold(id.GetType(), "isrc") {
			out.ISRC = id.GetId()
			break
		}
	}
	if al := t.GetAlbum(); al != nil {
		large, small := p.imagePair(al.GetCoverGroup(), al.GetCover())
		out.Album = &AlbumRef{
			URI:          uriFromGid(idTypeAlbum, al.GetGid()),
			Name:         al.GetName(),
			ImageURL:     large,
			ThumbnailURL: small,
			ReleaseDate:  formatDate(al.GetDate()),
			Artists:      artistRefs(al.GetArtist()),
		}
	}
	return out
}

func (p *AppPlayer) episodeDetailsFromProto(uri string, e *metadatapb.Episode) TrackDetails {
	if uri == "" {
		uri = uriFromGid(librespot.SpotifyIdTypeEpisode, e.GetGid())
	}
	out := TrackDetails{
		URI:        uri,
		Name:       e.GetName(),
		DurationMs: int64(e.GetDuration()),
		Explicit:   e.GetExplicit(),
		IsEpisode:  true,
		Artists:    []ArtistRef{},
	}
	large, small := p.imagePair(e.GetCoverImage(), nil)
	if show := e.GetShow(); show != nil {
		if large == "" {
			large, small = p.imagePair(show.GetCoverImage(), nil)
		}
		out.Album = &AlbumRef{
			URI:          uriFromGid(idTypeShow, show.GetGid()),
			Name:         show.GetName(),
			ImageURL:     large,
			ThumbnailURL: small,
			ReleaseDate:  formatDate(e.GetPublishTime()),
		}
		if show.GetPublisher() != "" {
			out.Artists = append(out.Artists, ArtistRef{Name: show.GetPublisher()})
		}
	}
	return out
}

func albumTypeString(a *metadatapb.Album) string {
	if s := a.GetTypeStr(); s != "" {
		return strings.ToLower(s)
	}
	if a.Type == nil {
		return ""
	}
	return strings.ToLower(a.GetType().String())
}

func (p *AppPlayer) albumSummaryFromProto(uri string, a *metadatapb.Album) AlbumSummary {
	if uri == "" {
		uri = uriFromGid(idTypeAlbum, a.GetGid())
	}
	large, small := p.imagePair(a.GetCoverGroup(), a.GetCover())
	total := 0
	for _, d := range a.GetDisc() {
		total += len(d.GetTrack())
	}
	return AlbumSummary{
		URI:          uri,
		Name:         a.GetName(),
		Type:         albumTypeString(a),
		ImageURL:     large,
		ThumbnailURL: small,
		ReleaseDate:  formatDate(a.GetDate()),
		Year:         int(a.GetDate().GetYear()),
		Artists:      artistRefs(a.GetArtist()),
		TotalTracks:  total,
	}
}

// ---------------------------------------------------------------------------
// Batched extended metadata
// ---------------------------------------------------------------------------

const (
	extMetadataChunkSize   = 400
	extMetadataConcurrency = 4
)

// fetchExtendedMetadata fetches kind for every uri in as few requests as
// possible and returns the decoded messages by uri. URIs the server does not
// know (or that fail to decode) are left out.
func fetchExtendedMetadata[T proto.Message](ctx context.Context, p *AppPlayer, uris []string, kind extmetadatapb.ExtensionKind, newMsg func() T) (map[string]T, error) {
	out := make(map[string]T, len(uris))
	if len(uris) == 0 {
		return out, nil
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, extMetadataConcurrency)
	)

	for start := 0; start < len(uris); start += extMetadataChunkSize {
		chunk := uris[start:min(start+extMetadataChunkSize, len(uris))]

		wg.Add(1)
		sem <- struct{}{}
		go func(chunk []string) {
			defer wg.Done()
			defer func() { <-sem }()

			req := &extmetadatapb.BatchedEntityRequest{EntityRequest: make([]*extmetadatapb.EntityRequest, 0, len(chunk))}
			for _, uri := range chunk {
				req.EntityRequest = append(req.EntityRequest, &extmetadatapb.EntityRequest{
					EntityUri: uri,
					Query:     []*extmetadatapb.ExtensionQuery{{ExtensionKind: kind}},
				})
			}

			resp, err := p.sess.Spclient().ExtendedMetadata(ctx, req)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}

			for _, arr := range resp.GetExtendedMetadata() {
				if arr.GetExtensionKind() != kind {
					continue
				}
				for _, d := range arr.GetExtensionData() {
					if d.GetHeader().GetStatusCode() != 200 || d.GetExtensionData() == nil {
						continue
					}
					msg := newMsg()
					if err := d.GetExtensionData().UnmarshalTo(msg); err != nil {
						continue
					}
					mu.Lock()
					out[d.GetEntityUri()] = msg
					mu.Unlock()
				}
			}
		}(chunk)
	}
	wg.Wait()

	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	if firstErr != nil {
		p.app.log.WithError(firstErr).Warnf("some extended metadata requests failed (%d of %d resolved)", len(out), len(uris))
	}
	return out, nil
}

// trackDetailsCache keeps recently resolved track details so queue snapshots
// and repeated lookups don't hit the network every time.
type trackDetailsCache struct {
	mu sync.Mutex
	m  map[string]TrackDetails
}

const trackDetailsCacheMax = 20000

func (c *trackDetailsCache) get(uri string) (TrackDetails, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.m[uri]
	return d, ok
}

func (c *trackDetailsCache) put(d TrackDetails) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= trackDetailsCacheMax {
		c.m = make(map[string]TrackDetails, 1024)
	}
	c.m[d.URI] = d
}

var trackCaches sync.Map // *App -> *trackDetailsCache

func (app *App) trackCache() *trackDetailsCache {
	v, _ := trackCaches.LoadOrStore(app, &trackDetailsCache{})
	return v.(*trackDetailsCache)
}

// GetTracksDetails resolves track and episode URIs to flat details using
// batched extended-metadata requests (hundreds of URIs per request). The result
// follows the order of uris, with duplicates and unknown URIs left out.
func (app *App) GetTracksDetails(ctx context.Context, p *AppPlayer, uris []string) ([]TrackDetails, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}

	cache := app.trackCache()
	found := make(map[string]TrackDetails, len(uris))
	var trackURIs, episodeURIs []string
	seen := make(map[string]struct{}, len(uris))
	for _, uri := range uris {
		uri = strings.TrimSpace(uri)
		if _, ok := seen[uri]; ok || uri == "" {
			continue
		}
		seen[uri] = struct{}{}

		if d, ok := cache.get(uri); ok {
			found[uri] = d
			continue
		}
		switch {
		case strings.HasPrefix(uri, "spotify:track:"):
			trackURIs = append(trackURIs, uri)
		case strings.HasPrefix(uri, "spotify:episode:"):
			episodeURIs = append(episodeURIs, uri)
		}
	}

	if len(trackURIs) > 0 {
		res, err := fetchExtendedMetadata(ctx, p, trackURIs, extmetadatapb.ExtensionKind_TRACK_V4, func() *metadatapb.Track { return &metadatapb.Track{} })
		if err != nil {
			return nil, fmt.Errorf("failed fetching track metadata: %w", err)
		}
		for uri, t := range res {
			d := p.trackDetailsFromProto(uri, t)
			cache.put(d)
			found[uri] = d
		}
	}
	if len(episodeURIs) > 0 {
		res, err := fetchExtendedMetadata(ctx, p, episodeURIs, extmetadatapb.ExtensionKind_EPISODE_V4, func() *metadatapb.Episode { return &metadatapb.Episode{} })
		if err != nil {
			return nil, fmt.Errorf("failed fetching episode metadata: %w", err)
		}
		for uri, e := range res {
			d := p.episodeDetailsFromProto(uri, e)
			cache.put(d)
			found[uri] = d
		}
	}

	out := make([]TrackDetails, 0, len(found))
	emitted := make(map[string]struct{}, len(found))
	for _, uri := range uris {
		uri = strings.TrimSpace(uri)
		if _, done := emitted[uri]; done {
			continue
		}
		if d, ok := found[uri]; ok {
			out = append(out, d)
			emitted[uri] = struct{}{}
		}
	}
	return out, nil
}

// GetAlbumsSummaries resolves album URIs to flat summaries in one batch,
// following the order of uris.
func (app *App) GetAlbumsSummaries(ctx context.Context, p *AppPlayer, uris []string) ([]AlbumSummary, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	res, err := fetchExtendedMetadata(ctx, p, dedupe(uris), extmetadatapb.ExtensionKind_ALBUM_V4, func() *metadatapb.Album { return &metadatapb.Album{} })
	if err != nil {
		return nil, fmt.Errorf("failed fetching album metadata: %w", err)
	}
	out := make([]AlbumSummary, 0, len(res))
	for _, uri := range dedupe(uris) {
		if a, ok := res[uri]; ok {
			out = append(out, p.albumSummaryFromProto(uri, a))
		}
	}
	return out, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// ---------------------------------------------------------------------------
// Album & artist details
// ---------------------------------------------------------------------------

// GetAlbumDetails returns an album with its full, flat track list.
func (app *App) GetAlbumDetails(ctx context.Context, p *AppPlayer, uri string) (*AlbumDetails, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	id, err := librespot.SpotifyIdFromUri(uri)
	if err != nil || id.Type() != idTypeAlbum {
		return nil, fmt.Errorf("not an album uri: %s", uri)
	}

	var album metadatapb.Album
	if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *id, extmetadatapb.ExtensionKind_ALBUM_V4, &album); err != nil {
		return nil, fmt.Errorf("failed fetching album: %w", err)
	}

	out := &AlbumDetails{
		AlbumSummary: p.albumSummaryFromProto(id.Uri(), &album),
		Label:        album.GetLabel(),
		Tracks:       []TrackDetails{},
	}
	for _, c := range album.GetCopyright() {
		if t := c.GetText(); t != "" {
			out.Copyrights = append(out.Copyrights, t)
		}
	}

	var trackURIs []string
	for _, disc := range album.GetDisc() {
		for _, t := range disc.GetTrack() {
			if u := uriFromGid(librespot.SpotifyIdTypeTrack, t.GetGid()); u != "" {
				trackURIs = append(trackURIs, u)
			}
		}
	}
	tracks, err := app.GetTracksDetails(ctx, p, trackURIs)
	if err != nil {
		return nil, err
	}
	out.Tracks = tracks
	return out, nil
}

// GetArtistDetails returns an artist with top tracks, discography and related
// artists, all resolved to flat entries.
func (app *App) GetArtistDetails(ctx context.Context, p *AppPlayer, uri string) (*ArtistDetails, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	id, err := librespot.SpotifyIdFromUri(uri)
	if err != nil || id.Type() != idTypeArtist {
		return nil, fmt.Errorf("not an artist uri: %s", uri)
	}

	var artist metadatapb.Artist
	if err := p.sess.Spclient().ExtendedMetadataSimple(ctx, *id, extmetadatapb.ExtensionKind_ARTIST_V4, &artist); err != nil {
		return nil, fmt.Errorf("failed fetching artist: %w", err)
	}

	out := &ArtistDetails{
		URI:          id.Uri(),
		Name:         artist.GetName(),
		TopTracks:    []TrackDetails{},
		Albums:       []AlbumSummary{},
		Singles:      []AlbumSummary{},
		Compilations: []AlbumSummary{},
		AppearsOn:    []AlbumSummary{},
		Related:      artistRefs(artist.GetRelated()),
	}
	out.ImageURL, out.ThumbnailURL = p.imagePair(artist.GetPortraitGroup(), artist.GetPortrait())
	if bios := artist.GetBiography(); len(bios) > 0 {
		out.Biography = bios[0].GetText()
	}

	// Top tracks are per country; prefer the account's country.
	country := ""
	if p.countryCode != nil {
		country = *p.countryCode
	}
	var top *metadatapb.TopTracks
	for _, tt := range artist.GetTopTrack() {
		if top == nil || strings.EqualFold(tt.GetCountry(), country) {
			top = tt
		}
	}
	var topURIs []string
	for _, t := range top.GetTrack() {
		if u := uriFromGid(librespot.SpotifyIdTypeTrack, t.GetGid()); u != "" {
			topURIs = append(topURIs, u)
		}
	}

	groupURIs := func(groups []*metadatapb.AlbumGroup) []string {
		var uris []string
		for _, g := range groups {
			// Each group holds one release in its regional variants; the first is enough.
			if albums := g.GetAlbum(); len(albums) > 0 {
				if u := uriFromGid(idTypeAlbum, albums[0].GetGid()); u != "" {
					uris = append(uris, u)
				}
			}
		}
		return uris
	}
	albumURIs := groupURIs(artist.GetAlbumGroup())
	singleURIs := groupURIs(artist.GetSingleGroup())
	compURIs := groupURIs(artist.GetCompilationGroup())
	appearsURIs := groupURIs(artist.GetAppearsOnGroup())

	all := append(append(append(append([]string{}, albumURIs...), singleURIs...), compURIs...), appearsURIs...)
	summaries, err := app.GetAlbumsSummaries(ctx, p, all)
	if err != nil {
		return nil, err
	}
	byURI := make(map[string]AlbumSummary, len(summaries))
	for _, s := range summaries {
		byURI[s.URI] = s
	}
	pick := func(uris []string) []AlbumSummary {
		out := make([]AlbumSummary, 0, len(uris))
		for _, u := range uris {
			if s, ok := byURI[u]; ok {
				out = append(out, s)
			}
		}
		return out
	}
	out.Albums = pick(albumURIs)
	out.Singles = pick(singleURIs)
	out.Compilations = pick(compURIs)
	out.AppearsOn = pick(appearsURIs)

	if out.TopTracks, err = app.GetTracksDetails(ctx, p, topURIs); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Lyrics
// ---------------------------------------------------------------------------

// GetLyrics fetches lyrics for a track. A track without lyrics returns a
// Lyrics value with no lines and a nil error.
func (app *App) GetLyrics(ctx context.Context, p *AppPlayer, uri string) (*Lyrics, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	id, err := librespot.SpotifyIdFromUri(uri)
	if err != nil || id.Type() != librespot.SpotifyIdTypeTrack {
		return nil, fmt.Errorf("not a track uri: %s", uri)
	}

	resp, err := p.sess.Spclient().Request(ctx, "GET", "/color-lyrics/v2/track/"+id.Base62(),
		url.Values{"format": {"json"}, "vocalRemoval": {"false"}, "market": {"from_token"}},
		http.Header{"App-Platform": {"WebPlayer"}, "Accept": {"application/json"}}, nil)
	if err != nil {
		return nil, fmt.Errorf("lyrics request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := &Lyrics{URI: id.Uri(), SyncType: "UNSYNCED", Lines: []LyricsLine{}}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return out, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading lyrics: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden {
		// Spotify rejects color-lyrics for this client's token regardless of
		// App-Platform or the image path (verified: every valid platform
		// returns 403 on a Premium account).
		return nil, fmt.Errorf("lyrics are not available to this client (Spotify returned 403)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lyrics returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var raw struct {
		Lyrics struct {
			SyncType string `json:"syncType"`
			Language string `json:"language"`
			Provider string `json:"provider"`
			Lines    []struct {
				StartTimeMs string `json:"startTimeMs"`
				EndTimeMs   string `json:"endTimeMs"`
				Words       string `json:"words"`
			} `json:"lines"`
		} `json:"lyrics"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed decoding lyrics: %w", err)
	}

	if raw.Lyrics.SyncType != "" {
		out.SyncType = raw.Lyrics.SyncType
	}
	out.Language = raw.Lyrics.Language
	out.Provider = raw.Lyrics.Provider
	for _, l := range raw.Lyrics.Lines {
		start, _ := strconv.ParseInt(l.StartTimeMs, 10, 64)
		end, _ := strconv.ParseInt(l.EndTimeMs, 10, 64)
		out.Lines = append(out.Lines, LyricsLine{StartMs: start, EndMs: end, Words: l.Words})
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
