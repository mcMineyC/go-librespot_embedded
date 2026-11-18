package daemoncore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Spotify's collection v2 service stores the user's saved items in named sets:
// "collection" holds liked tracks and saved albums, "artist" followed artists,
// "show" followed podcasts. The protobuf schema (collection2v2) is not part of
// this repo's generated protos, so the few small messages are encoded by hand.

const collectionContentType = "application/vnd.collection-v2.spotify.proto"

// CollectionItem is a saved entry. AddedAt is unix seconds.
type CollectionItem struct {
	URI     string `json:"uri"`
	AddedAt int64  `json:"added_at"`
}

// SavedAlbum is a saved album with its metadata resolved.
type SavedAlbum struct {
	AlbumSummary
	AddedAt int64 `json:"added_at"`
}

type collectionPage struct {
	items     []collectionPageItem
	nextToken string
	syncToken string
}

type collectionPageItem struct {
	uri       string
	addedAt   int64
	isRemoved bool
}

func encodeCollectionPageRequest(username, set, token string, limit int) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, username)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, set)
	if token != "" {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendString(b, token)
	}
	b = protowire.AppendTag(b, 4, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(limit))
	return b
}

func encodeCollectionItem(uri string, addedAt int64, removed bool) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, uri)
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(addedAt))
	if removed {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	return b
}

func encodeCollectionWriteRequest(username, set string, uris []string, removed bool) []byte {
	now := time.Now().Unix()
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, username)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, set)
	for _, uri := range uris {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, encodeCollectionItem(uri, now, removed))
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	b = protowire.AppendTag(b, 4, protowire.BytesType)
	b = protowire.AppendString(b, hex.EncodeToString(id))
	return b
}

// encodeCollectionContainsRequest encodes collection2v2 ContainsRequest:
// username = 1, set = 2, repeated items = 3, where each item is a message
// with uri = 1. The response is a packed repeated bool (field 1), one per
// item in request order. (Verified against the live endpoint; plain-string
// items get a 400.)
func encodeCollectionContainsRequest(username, set string, uris []string) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, username)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, set)
	for _, uri := range uris {
		var item []byte
		item = protowire.AppendTag(item, 1, protowire.BytesType)
		item = protowire.AppendString(item, uri)
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, item)
	}
	return b
}

// walkProto calls fn for every top-level field in b.
func walkProto(b []byte, fn func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			x, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			if err := fn(num, typ, nil, x); err != nil {
				return err
			}
			b = b[m:]
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			if err := fn(num, typ, v, 0); err != nil {
				return err
			}
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			b = b[m:]
		}
	}
	return nil
}

func decodeCollectionPage(b []byte) (*collectionPage, error) {
	page := &collectionPage{}
	err := walkProto(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			var item collectionPageItem
			if err := walkProto(v, func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error {
				switch {
				case num == 1 && typ == protowire.BytesType:
					item.uri = string(v)
				case num == 2 && typ == protowire.VarintType:
					item.addedAt = int64(int32(x))
				case num == 3 && typ == protowire.VarintType:
					item.isRemoved = x != 0
				}
				return nil
			}); err != nil {
				return err
			}
			page.items = append(page.items, item)
		case num == 2 && typ == protowire.BytesType:
			page.nextToken = string(v)
		case num == 3 && typ == protowire.BytesType:
			page.syncToken = string(v)
		}
		return nil
	})
	return page, err
}

func (app *App) collectionRequest(ctx context.Context, p *AppPlayer, path string, body []byte) ([]byte, error) {
	if p == nil || p.sess == nil {
		return nil, fmt.Errorf("player session not initialized")
	}
	resp, err := p.sess.Spclient().Request(ctx, "POST", path, nil, http.Header{
		"Content-Type": {collectionContentType},
		"Accept":       {collectionContentType},
	}, body)
	if err != nil {
		return nil, fmt.Errorf("collection request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading collection response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("collection %s returned %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// GetCollectionSet returns every item in a collection set, newest first.
func (app *App) GetCollectionSet(ctx context.Context, p *AppPlayer, set string) ([]CollectionItem, error) {
	username := p.sess.Username()
	if username == "" {
		return nil, fmt.Errorf("session username is empty")
	}

	var out []CollectionItem
	token := ""
	for page := 0; page < 1000; page++ {
		raw, err := app.collectionRequest(ctx, p, "/collection/v2/paging", encodeCollectionPageRequest(username, set, token, 300))
		if err != nil {
			return nil, err
		}
		resp, err := decodeCollectionPage(raw)
		if err != nil {
			return nil, fmt.Errorf("failed decoding collection page: %w", err)
		}
		for _, it := range resp.items {
			if it.isRemoved || it.uri == "" {
				continue
			}
			out = append(out, CollectionItem{URI: it.uri, AddedAt: it.addedAt})
		}
		if resp.nextToken == "" || resp.nextToken == token || len(resp.items) == 0 {
			break
		}
		token = resp.nextToken
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt > out[j].AddedAt })
	return out, nil
}

func filterCollection(items []CollectionItem, prefix string) []CollectionItem {
	out := make([]CollectionItem, 0, len(items))
	for _, it := range items {
		if strings.HasPrefix(it.URI, prefix) {
			out = append(out, it)
		}
	}
	return out
}

// GetLikedTracks returns the Liked Songs (track URIs and when they were
// added), newest first. Resolve metadata with GetTracksDetails.
func (app *App) GetLikedTracks(ctx context.Context, p *AppPlayer) ([]CollectionItem, error) {
	items, err := app.GetCollectionSet(ctx, p, "collection")
	if err != nil {
		return nil, err
	}
	return filterCollection(items, "spotify:track:"), nil
}

// GetSavedAlbums returns saved albums with metadata, newest first.
func (app *App) GetSavedAlbums(ctx context.Context, p *AppPlayer) ([]SavedAlbum, error) {
	items, err := app.GetCollectionSet(ctx, p, "collection")
	if err != nil {
		return nil, err
	}
	items = filterCollection(items, "spotify:album:")

	uris := make([]string, len(items))
	for i, it := range items {
		uris[i] = it.URI
	}
	summaries, err := app.GetAlbumsSummaries(ctx, p, uris)
	if err != nil {
		return nil, err
	}
	byURI := make(map[string]AlbumSummary, len(summaries))
	for _, s := range summaries {
		byURI[s.URI] = s
	}

	out := make([]SavedAlbum, 0, len(items))
	for _, it := range items {
		s, ok := byURI[it.URI]
		if !ok {
			s = AlbumSummary{URI: it.URI}
		}
		out = append(out, SavedAlbum{AlbumSummary: s, AddedAt: it.AddedAt})
	}
	return out, nil
}

// collectionSetForURI picks the collection set an item is saved in.
func collectionSetForURI(uri string) (string, error) {
	switch {
	case strings.HasPrefix(uri, "spotify:track:"), strings.HasPrefix(uri, "spotify:album:"):
		return "collection", nil
	case strings.HasPrefix(uri, "spotify:artist:"):
		return "artist", nil
	case strings.HasPrefix(uri, "spotify:show:"):
		return "show", nil
	case strings.HasPrefix(uri, "spotify:episode:"):
		return "listenlater", nil
	default:
		return "", fmt.Errorf("cannot save uri: %s", uri)
	}
}

func groupBySet(uris []string) (map[string][]string, error) {
	groups := map[string][]string{}
	for _, uri := range dedupe(uris) {
		set, err := collectionSetForURI(uri)
		if err != nil {
			return nil, err
		}
		groups[set] = append(groups[set], uri)
	}
	return groups, nil
}

// SetSaved likes (saved=true) or unlikes tracks, and saves or removes albums,
// artists (follow), shows and episodes (Your Episodes).
func (app *App) SetSaved(ctx context.Context, p *AppPlayer, uris []string, saved bool) error {
	username := p.sess.Username()
	if username == "" {
		return fmt.Errorf("session username is empty")
	}
	groups, err := groupBySet(uris)
	if err != nil {
		return err
	}
	for set, group := range groups {
		if _, err := app.collectionRequest(ctx, p, "/collection/v2/write", encodeCollectionWriteRequest(username, set, group, !saved)); err != nil {
			return err
		}
	}
	return nil
}

// AreSaved reports, for each uri, whether it is in the user's library.
func (app *App) AreSaved(ctx context.Context, p *AppPlayer, uris []string) (map[string]bool, error) {
	username := p.sess.Username()
	if username == "" {
		return nil, fmt.Errorf("session username is empty")
	}
	groups, err := groupBySet(uris)
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(uris))
	for set, group := range groups {
		raw, err := app.collectionRequest(ctx, p, "/collection/v2/contains", encodeCollectionContainsRequest(username, set, group))
		if err != nil {
			return nil, err
		}
		var found []bool
		if err := walkProto(raw, func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error {
			if num != 1 {
				return nil
			}
			if typ == protowire.VarintType {
				found = append(found, x != 0)
				return nil
			}
			// packed repeated bool
			for len(v) > 0 {
				x, n := protowire.ConsumeVarint(v)
				if n < 0 {
					return protowire.ParseError(n)
				}
				found = append(found, x != 0)
				v = v[n:]
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("failed decoding contains response: %w", err)
		}
		for i, uri := range group {
			out[uri] = i < len(found) && found[i]
		}
	}
	return out, nil
}
