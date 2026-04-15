// Fully refactored: NO spotcontrol dependency
// Includes local URI parsing + base62->hex conversion everywhere

package spclient

import (
	"context"
	"fmt"
	"io"
	"net/url"

	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	playlist4pb "github.com/devgianlu/go-librespot/proto/spotify/playlist4"
	proto "google.golang.org/protobuf/proto"
)

// ===================== CORE HELPERS =====================

func (c *Spclient) hexFromURI(uri string, expected SpotifyIDType) (string, error) {
	typ, base62, err := NormalizeSpotifyURI(uri)
	if err != nil {
		return "", err
	}
	if expected != "" && typ != expected {
		return "", fmt.Errorf("expected %s but got %s", expected, typ)
	}
	return Base62ToHex(base62)
}

func (c *Spclient) base62FromURI(uri string, expected SpotifyIDType) (string, error) {
	typ, base62, err := NormalizeSpotifyURI(uri)
	if err != nil {
		return "", err
	}
	if expected != "" && typ != expected {
		return "", fmt.Errorf("expected %s but got %s", expected, typ)
	}
	return base62, nil
}

// ===================== METADATA CORE =====================

func (c *Spclient) getMetadataRaw(ctx context.Context, entityType, hexGid string) ([]byte, error) {
	path := fmt.Sprintf("/metadata/4/%s/%s", entityType, hexGid)
	resp, err := c.Request(ctx, "GET", path, url.Values{"market": {"from_token"}}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("metadata error %d: %s", resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// ===================== TRACK =====================

func (c *Spclient) GetTrackMetadataProto(ctx context.Context, hexID string) (*metadatapb.Track, error) {
	b, err := c.getMetadataRaw(ctx, "track", hexID)
	if err != nil {
		return nil, err
	}
	var track metadatapb.Track
	if err := proto.Unmarshal(b, &track); err != nil {
		return nil, fmt.Errorf("failed to decode track proto: %w", err)
	}

	return &track, nil
}

func (c *Spclient) GetTrackMetadataByURI(ctx context.Context, uri string) (*metadatapb.Track, error) {
	hexID, err := c.hexFromURI(uri, TypeTrack)
	if err != nil {
		return nil, err
	}
	return c.GetTrackMetadataProto(ctx, hexID)
}

// ===================== ALBUM =====================

func (c *Spclient) GetAlbumMetadata(ctx context.Context, hexID string) (*metadatapb.Album, error) {
	b, err := c.getMetadataRaw(ctx, "album", hexID)
	if err != nil {
		return nil, err
	}
	var album metadatapb.Album
	if err := proto.Unmarshal(b, &album); err != nil {
		return nil, fmt.Errorf("failed to decode album proto: %w", err)
	}
	return &album, nil
}

func (c *Spclient) GetAlbumMetadataByURI(ctx context.Context, uri string) (*metadatapb.Album, error) {
	hexID, err := c.hexFromURI(uri, TypeAlbum)
	if err != nil {
		return nil, err
	}
	return c.GetAlbumMetadata(ctx, hexID)
}

func (c *Spclient) GetAlbumTracks(ctx context.Context, hexID string) ([]*metadatapb.Track, error) {
	album, err := c.GetAlbumMetadata(ctx, hexID)
	if err != nil {
		return nil, err
	}
	var tracks []*metadatapb.Track
	for _, d := range album.GetDisc() {
		tracks = append(tracks, d.GetTrack()...)
	}
	return tracks, nil
}

func (c *Spclient) GetAlbumTracksByURI(ctx context.Context, uri string) ([]*metadatapb.Track, error) {
	hexID, err := c.hexFromURI(uri, TypeAlbum)
	if err != nil {
		return nil, err
	}
	return c.GetAlbumTracks(ctx, hexID)
}

// ===================== ARTIST =====================

func (c *Spclient) GetArtistMetadata(ctx context.Context, hexID string) (*metadatapb.Artist, error) {
	b, err := c.getMetadataRaw(ctx, "artist", hexID)
	if err != nil {
		return nil, err
	}
	var artist metadatapb.Artist
	if err := proto.Unmarshal(b, &artist); err != nil {
		return nil, fmt.Errorf("failed to decode artist proto: %w", err)
	}
	return &artist, nil
}

func (c *Spclient) GetArtistMetadataByURI(ctx context.Context, uri string) (*metadatapb.Artist, error) {
	hexID, err := c.hexFromURI(uri, TypeArtist)
	if err != nil {
		return nil, err
	}
	return c.GetArtistMetadata(ctx, hexID)
}

func (c *Spclient) GetArtistDiscography(ctx context.Context, hexID string) ([]*metadatapb.Album, error) {
	artist, err := c.GetArtistMetadata(ctx, hexID)
	if err != nil {
		return nil, err
	}
	var out []*metadatapb.Album
	for _, g := range artist.GetAlbumGroup() {
		out = append(out, g.GetAlbum()...)
	}
	for _, g := range artist.GetSingleGroup() {
		out = append(out, g.GetAlbum()...)
	}
	for _, g := range artist.GetCompilationGroup() {
		out = append(out, g.GetAlbum()...)
	}
	for _, g := range artist.GetAppearsOnGroup() {
		out = append(out, g.GetAlbum()...)
	}
	return out, nil
}

func (c *Spclient) GetArtistDiscographyByURI(ctx context.Context, uri string) ([]*metadatapb.Album, error) {
	hexID, err := c.hexFromURI(uri, TypeArtist)
	if err != nil {
		return nil, err
	}
	return c.GetArtistDiscography(ctx, hexID)
}

// ===================== PLAYLIST =====================

func (c *Spclient) GetPlaylistContents(ctx context.Context, base62 string) (*playlist4pb.SelectedListContent, error) {
	resp, err := c.Request(ctx, "GET",
		fmt.Sprintf("/playlist/v2/playlist/%s", base62),
		nil, nil, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("playlist request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("playlist returned %d: %s", resp.StatusCode, string(body))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading playlist response: %w", err)
	}

	var content playlist4pb.SelectedListContent
	if err := proto.Unmarshal(b, &content); err != nil {
		return nil, fmt.Errorf("failed to decode playlist proto: %w", err)
	}

	return &content, nil
}
func (c *Spclient) GetPlaylistContentsByURI(ctx context.Context, uri string) (*playlist4pb.SelectedListContent, error) {
	id, err := c.base62FromURI(uri, TypePlaylist)
	if err != nil {
		return nil, err
	}
	return c.GetPlaylistContents(ctx, id)
}

func (c *Spclient) GetPlaylistTracks(ctx context.Context, base62 string) ([]string, error) {
	content, err := c.GetPlaylistContents(ctx, base62)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, i := range content.GetContents().GetItems() {
		out = append(out, i.GetUri())
	}
	return out, nil
}

func (c *Spclient) GetPlaylistTracksByURI(ctx context.Context, uri string) ([]string, error) {
	id, err := c.base62FromURI(uri, TypePlaylist)
	if err != nil {
		return nil, err
	}
	return c.GetPlaylistTracks(ctx, id)
}

// ===================== GENERIC =====================

func (c *Spclient) GetMetadataForURI(ctx context.Context, uri string) (any, error) {
	typ, base62, err := NormalizeSpotifyURI(uri)
	if err != nil {
		return nil, err
	}

	switch typ {
	case TypeTrack:
		hexID, _ := Base62ToHex(base62)
		t, err := c.GetTrackMetadataProto(ctx, hexID)
		if err != nil {
			return nil, err
		}
		return t, nil

	case TypeAlbum:
		hexID, _ := Base62ToHex(base62)
		a, err := c.GetAlbumMetadata(ctx, hexID)
		if err != nil {
			return nil, err
		}
		return a, nil

	case TypeArtist:
		hexID, _ := Base62ToHex(base62)
		a, err := c.GetArtistMetadata(ctx, hexID)
		if err != nil {
			return nil, err
		}
		return a, nil

	case TypePlaylist:
		hexID, _ := Base62ToHex(base62)
		p, err := c.GetPlaylistContents(ctx, hexID)
		if err != nil {
			return nil, err
		}
		return p, nil

	default:
		return nil, fmt.Errorf("unsupported URI type %s", typ)
	}
}
