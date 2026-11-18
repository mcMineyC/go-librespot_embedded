package daemoncore

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	playlist4pb "github.com/devgianlu/go-librespot/proto/spotify/playlist4"
	"google.golang.org/protobuf/proto"
)

// PlaylistTrack is one playlist entry. AddedAt is unix milliseconds.
type PlaylistTrack struct {
	URI     string `json:"uri"`
	AddedAt int64  `json:"added_at,omitempty"`
	AddedBy string `json:"added_by,omitempty"`
	ItemID  string `json:"item_id,omitempty"`
}

// Playlist is a playlist's attributes and track list. Revision changes
// whenever the playlist changes, so a host can store it and skip unchanged
// playlists on the next sync.
type Playlist struct {
	URI           string `json:"uri"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	ImageURL      string `json:"image_url,omitempty"`
	Owner         string `json:"owner,omitempty"`
	Collaborative bool   `json:"collaborative"`
	Revision      string `json:"revision"`
	Length        int    `json:"length"`
	// Unchanged is true when the caller's known revision matches; Tracks is
	// then left empty.
	Unchanged bool            `json:"unchanged"`
	Tracks    []PlaylistTrack `json:"tracks"`
}

const playlistPageSize = 1000

func playlistBase62(uri string) (string, error) {
	id, err := librespot.SpotifyIdFromUri(uri)
	if err != nil || id.Type() != librespot.SpotifyIdTypePlaylist {
		return "", fmt.Errorf("not a playlist uri: %s", uri)
	}
	return id.Base62(), nil
}

func (app *App) playlistRequest(ctx context.Context, p *AppPlayer, method, path string, query url.Values, body []byte) (*playlist4pb.SelectedListContent, []byte, error) {
	if p == nil || p.sess == nil {
		return nil, nil, fmt.Errorf("player session not initialized")
	}
	resp, err := p.sess.Spclient().Request(ctx, method, path, query, http.Header{"Accept": {"application/x-protobuf"}}, body)
	if err != nil {
		return nil, nil, fmt.Errorf("playlist request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed reading playlist response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, raw, fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, truncate(string(raw), 200))
	}

	var out playlist4pb.SelectedListContent
	if len(raw) > 0 {
		if err := proto.Unmarshal(raw, &out); err != nil {
			return nil, raw, fmt.Errorf("failed decoding playlist response: %w", err)
		}
	}
	return &out, raw, nil
}

func (app *App) fetchPlaylistPage(ctx context.Context, p *AppPlayer, base62 string, from, length int) (*playlist4pb.SelectedListContent, error) {
	out, _, err := app.playlistRequest(ctx, p, "GET", "/playlist/v2/playlist/"+base62, url.Values{
		"decorate": {"revision,length,attributes,timestamp,owner"},
		"from":     {strconv.Itoa(from)},
		"length":   {strconv.Itoa(length)},
	}, nil)
	return out, err
}

func bestPictureURL(p *AppPlayer, attrs *playlist4pb.ListAttributes) string {
	if attrs == nil {
		return ""
	}
	rank := func(name string) int {
		switch strings.ToLower(name) {
		case "large":
			return 0
		case "xlarge":
			return 1
		case "default":
			return 2
		case "small":
			return 3
		}
		return 9
	}
	best, bestRank := "", 99
	for _, ps := range attrs.GetPictureSize() {
		if u := strings.TrimSpace(ps.GetUrl()); u != "" && rank(ps.GetTargetName()) < bestRank {
			best, bestRank = u, rank(ps.GetTargetName())
		}
	}
	if best == "" && len(attrs.GetPicture()) > 0 {
		best = p.imageURLForFileID(attrs.GetPicture())
	}
	return best
}

// GetPlaylist returns a playlist's attributes and all its tracks. If
// knownRevision matches the current revision, only the attributes are
// returned with Unchanged set.
func (app *App) GetPlaylist(ctx context.Context, p *AppPlayer, uri, knownRevision string) (*Playlist, error) {
	base62, err := playlistBase62(uri)
	if err != nil {
		return nil, err
	}

	first, err := app.fetchPlaylistPage(ctx, p, base62, 0, playlistPageSize)
	if err != nil {
		return nil, err
	}

	attrs := first.GetAttributes()
	out := &Playlist{
		URI:           "spotify:playlist:" + base62,
		Name:          attrs.GetName(),
		Description:   cleanDescription(attrs.GetDescription()),
		ImageURL:      bestPictureURL(p, attrs),
		Owner:         first.GetOwnerUsername(),
		Collaborative: attrs.GetCollaborative(),
		Revision:      hex.EncodeToString(first.GetRevision()),
		Length:        int(first.GetLength()),
		Tracks:        []PlaylistTrack{},
	}
	if knownRevision != "" && strings.EqualFold(knownRevision, out.Revision) {
		out.Unchanged = true
		return out, nil
	}

	appendItems := func(items []*playlist4pb.Item) {
		for _, it := range items {
			if it.GetUri() == "" {
				continue
			}
			a := it.GetAttributes()
			t := PlaylistTrack{URI: it.GetUri(), AddedAt: a.GetTimestamp(), AddedBy: a.GetAddedBy()}
			if id := a.GetItemId(); len(id) > 0 {
				t.ItemID = hex.EncodeToString(id)
			}
			out.Tracks = append(out.Tracks, t)
		}
	}
	appendItems(first.GetContents().GetItems())

	for len(out.Tracks) < out.Length {
		before := len(out.Tracks)
		page, err := app.fetchPlaylistPage(ctx, p, base62, before, playlistPageSize)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(page.GetRevision(), first.GetRevision()) && len(page.GetRevision()) > 0 {
			// Changed while paging: start over once from the new revision.
			return app.GetPlaylist(ctx, p, uri, "")
		}
		appendItems(page.GetContents().GetItems())
		if len(out.Tracks) == before {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Editing
// ---------------------------------------------------------------------------

func changeInfo() *playlist4pb.ChangeInfo {
	return &playlist4pb.ChangeInfo{
		Timestamp: proto.Int64(time.Now().UnixMilli()),
		Source: &playlist4pb.SourceInfo{
			Client: playlist4pb.SourceInfo_CLIENT.Enum(),
		},
	}
}

func (app *App) currentRevision(ctx context.Context, p *AppPlayer, path string) ([]byte, error) {
	out, _, err := app.playlistRequest(ctx, p, "GET", path, url.Values{
		"decorate": {"revision"},
		"from":     {"0"},
		"length":   {"1"},
	}, nil)
	if err != nil {
		return nil, err
	}
	return out.GetRevision(), nil
}

// applyChanges posts ops against the list at path (a playlist or the
// rootlist) on top of its current revision and returns the new revision.
func (app *App) applyChanges(ctx context.Context, p *AppPlayer, path string, ops ...*playlist4pb.Op) (string, error) {
	base, err := app.currentRevision(ctx, p, path)
	if err != nil {
		return "", err
	}
	return app.applyChangesAt(ctx, p, path, base, ops...)
}

// applyChangesAt is applyChanges on top of a revision the caller already has.
func (app *App) applyChangesAt(ctx context.Context, p *AppPlayer, path string, base []byte, ops ...*playlist4pb.Op) (string, error) {
	body, err := proto.Marshal(&playlist4pb.ListChanges{
		BaseRevision:           base,
		Deltas:                 []*playlist4pb.Delta{{Ops: ops, Info: changeInfo()}},
		WantResultingRevisions: proto.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("failed encoding playlist changes: %w", err)
	}

	out, _, err := app.playlistRequest(ctx, p, "POST", path+"/changes", nil, body)
	if err != nil {
		return "", err
	}
	if revs := out.GetResultingRevisions(); len(revs) > 0 {
		return hex.EncodeToString(revs[len(revs)-1]), nil
	}
	return hex.EncodeToString(out.GetRevision()), nil
}

func (app *App) rootlistPath(p *AppPlayer) (string, error) {
	username := p.sess.Username()
	if username == "" {
		return "", fmt.Errorf("session username is empty")
	}
	return fmt.Sprintf("/playlist/v2/user/%s/rootlist", url.PathEscape(username)), nil
}

func newItems(uris []string) []*playlist4pb.Item {
	now := time.Now().UnixMilli()
	items := make([]*playlist4pb.Item, 0, len(uris))
	for _, uri := range uris {
		items = append(items, &playlist4pb.Item{
			Uri:        proto.String(uri),
			Attributes: &playlist4pb.ItemAttributes{Timestamp: proto.Int64(now)},
		})
	}
	return items
}

// AddToPlaylist inserts tracks at position (negative appends) and returns the
// new revision.
func (app *App) AddToPlaylist(ctx context.Context, p *AppPlayer, playlistURI string, uris []string, position int) (string, error) {
	base62, err := playlistBase62(playlistURI)
	if err != nil {
		return "", err
	}
	if len(uris) == 0 {
		return "", fmt.Errorf("no tracks given")
	}
	add := &playlist4pb.Add{Items: newItems(uris)}
	if position < 0 {
		add.AddLast = proto.Bool(true)
	} else {
		add.FromIndex = proto.Int32(int32(position))
	}
	return app.applyChanges(ctx, p, "/playlist/v2/playlist/"+base62,
		&playlist4pb.Op{Kind: playlist4pb.Op_ADD.Enum(), Add: add})
}

// RemoveFromPlaylist removes the entry at index (0-based) and returns the new
// revision. If expectedURI is non-empty, the entry at index must be that URI;
// otherwise nothing is removed and an error is returned, which protects
// against removing the wrong track when the playlist changed since the caller
// last read it.
func (app *App) RemoveFromPlaylist(ctx context.Context, p *AppPlayer, playlistURI string, index int, expectedURI string) (string, error) {
	base62, err := playlistBase62(playlistURI)
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "", fmt.Errorf("index must not be negative")
	}

	// Read the entry and the revision in one request, so the removal is
	// applied against exactly what was checked.
	page, err := app.fetchPlaylistPage(ctx, p, base62, index, 1)
	if err != nil {
		return "", err
	}
	if index >= int(page.GetLength()) {
		return "", fmt.Errorf("index %d out of range (playlist has %d entries)", index, page.GetLength())
	}
	items := page.GetContents().GetItems()
	if len(items) == 0 {
		return "", fmt.Errorf("no entry returned at index %d", index)
	}
	item := items[0]
	if expectedURI != "" && item.GetUri() != expectedURI {
		return "", fmt.Errorf("entry at index %d is %s, not %s", index, item.GetUri(), expectedURI)
	}

	return app.applyChangesAt(ctx, p, "/playlist/v2/playlist/"+base62, page.GetRevision(), &playlist4pb.Op{
		Kind: playlist4pb.Op_REM.Enum(),
		Rem: &playlist4pb.Rem{
			FromIndex: proto.Int32(int32(index)),
			Length:    proto.Int32(1),
			Items:     []*playlist4pb.Item{{Uri: proto.String(item.GetUri())}},
		},
	})
}

// RenamePlaylist changes the name and, if description is non-nil, the
// description. It returns the new revision.
func (app *App) RenamePlaylist(ctx context.Context, p *AppPlayer, playlistURI, name string, description *string) (string, error) {
	base62, err := playlistBase62(playlistURI)
	if err != nil {
		return "", err
	}
	attrs := &playlist4pb.ListAttributes{}
	if name != "" {
		attrs.Name = proto.String(name)
	}
	if description != nil {
		attrs.Description = proto.String(*description)
	}
	if attrs.Name == nil && attrs.Description == nil {
		return "", fmt.Errorf("nothing to change")
	}
	return app.applyChanges(ctx, p, "/playlist/v2/playlist/"+base62, &playlist4pb.Op{
		Kind: playlist4pb.Op_UPDATE_LIST_ATTRIBUTES.Enum(),
		UpdateListAttributes: &playlist4pb.UpdateListAttributes{
			NewAttributes: &playlist4pb.ListAttributesPartialState{Values: attrs},
		},
	})
}

// CreatePlaylist creates a playlist, adds it to the top of the user's library
// and returns its uri.
func (app *App) CreatePlaylist(ctx context.Context, p *AppPlayer, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name must not be empty")
	}
	body, err := proto.Marshal(&playlist4pb.ListUpdateRequest{
		Attributes: &playlist4pb.ListAttributes{Name: proto.String(name)},
		Info:       changeInfo(),
	})
	if err != nil {
		return "", fmt.Errorf("failed encoding create request: %w", err)
	}

	if p == nil || p.sess == nil {
		return "", fmt.Errorf("player session not initialized")
	}
	resp, err := p.sess.Spclient().Request(ctx, "POST", "/playlist/v2/playlist", nil, http.Header{"Accept": {"application/x-protobuf"}}, body)
	if err != nil {
		return "", fmt.Errorf("create playlist request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed reading create playlist response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create playlist returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var reply playlist4pb.CreateListReply
	if err := proto.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("failed decoding create playlist response: %w", err)
	}
	uri := reply.GetUri()
	if uri == "" {
		return "", fmt.Errorf("create playlist returned no uri")
	}
	// The create endpoint ignores the attributes in the request (the new
	// playlist comes back unnamed), so set the name with an explicit update.
	if _, err := app.RenamePlaylist(ctx, p, uri, name, nil); err != nil {
		return uri, fmt.Errorf("playlist %s created but naming it failed: %w", uri, err)
	}

	root, err := app.rootlistPath(p)
	if err != nil {
		return "", err
	}
	if _, err := app.applyChanges(ctx, p, root, &playlist4pb.Op{
		Kind: playlist4pb.Op_ADD.Enum(),
		Add:  &playlist4pb.Add{Items: newItems([]string{uri}), AddFirst: proto.Bool(true)},
	}); err != nil {
		return uri, fmt.Errorf("playlist %s created but not added to library: %w", uri, err)
	}
	return uri, nil
}

// DeletePlaylist removes a playlist from the user's library (for playlists the
// user owns this is what Spotify clients call "delete").
func (app *App) DeletePlaylist(ctx context.Context, p *AppPlayer, playlistURI string) error {
	if _, err := playlistBase62(playlistURI); err != nil {
		return err
	}
	root, err := app.rootlistPath(p)
	if err != nil {
		return err
	}
	_, err = app.applyChanges(ctx, p, root, &playlist4pb.Op{
		Kind: playlist4pb.Op_REM.Enum(),
		Rem:  &playlist4pb.Rem{Items: []*playlist4pb.Item{{Uri: proto.String(playlistURI)}}, ItemsAsKey: proto.Bool(true)},
	})
	return err
}
