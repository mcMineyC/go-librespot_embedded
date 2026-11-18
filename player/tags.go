package player

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/vorbis"
)

const coverImageBaseUrl = "https://i.scdn.co/image/"

// maxCoverSize caps how much image data is downloaded and embedded.
const maxCoverSize = 4 << 20

// VorbisComments returns the "KEY=value" tags describing media. Spotify-specific
// identifiers use the custom SPOTIFY_URI and SPOTIFY_ID keys. rg may be nil.
func VorbisComments(media *librespot.Media, rg *vorbis.ReplayGain) []string {
	var c []string
	add := func(key, val string) {
		if val != "" {
			c = append(c, key+"="+val)
		}
	}

	id := media.Id()
	add("SPOTIFY_URI", id.Uri())
	add("SPOTIFY_ID", id.Base62())

	if media.IsTrack() {
		track := media.Track()
		add("TITLE", track.GetName())
		for _, a := range track.Artist {
			add("ARTIST", a.GetName())
		}
		if album := track.GetAlbum(); album != nil {
			add("ALBUM", album.GetName())
			for _, a := range album.Artist {
				add("ALBUMARTIST", a.GetName())
			}
			add("DATE", formatDate(album.GetDate()))
			add("LABEL", album.GetLabel())
		}
		if n := track.GetNumber(); n > 0 {
			add("TRACKNUMBER", strconv.Itoa(int(n)))
		}
		if n := track.GetDiscNumber(); n > 0 {
			add("DISCNUMBER", strconv.Itoa(int(n)))
		}
		for _, ext := range track.ExternalId {
			if strings.EqualFold(ext.GetType(), "isrc") {
				add("ISRC", strings.ToUpper(ext.GetId()))
			}
		}
	} else {
		episode := media.Episode()
		add("TITLE", episode.GetName())
		if show := episode.GetShow(); show != nil {
			add("ALBUM", show.GetName())
			add("ARTIST", show.GetPublisher())
		}
	}

	if rg != nil {
		c = append(c, rg.Comments()...)
	}
	return c
}

func formatDate(d *metadatapb.Date) string {
	if d == nil || d.GetYear() == 0 {
		return ""
	}
	if d.GetMonth() == 0 {
		return fmt.Sprintf("%04d", d.GetYear())
	}
	if d.GetDay() == 0 {
		return fmt.Sprintf("%04d-%02d", d.GetYear(), d.GetMonth())
	}
	return fmt.Sprintf("%04d-%02d-%02d", d.GetYear(), d.GetMonth(), d.GetDay())
}

// CoverUrl returns the URL of the largest cover image for media, or "" if none.
func CoverUrl(media *librespot.Media) string {
	var images []*metadatapb.Image
	if media.IsTrack() {
		album := media.Track().GetAlbum()
		images = album.GetCover()
		if len(images) == 0 {
			images = album.GetCoverGroup().GetImage()
		}
	} else {
		images = media.Episode().GetCoverImage().GetImage()
	}

	var best *metadatapb.Image
	for _, img := range images {
		if len(img.FileId) == 0 {
			continue
		}
		if best == nil || img.GetSize() > best.GetSize() {
			best = img
		}
	}
	if best == nil {
		return ""
	}
	return coverImageBaseUrl + hex.EncodeToString(best.FileId)
}

// FetchCover downloads the cover image for media. It returns nil, nil when the
// media has no cover.
func FetchCover(ctx context.Context, client *http.Client, media *librespot.Media) (*vorbis.Picture, error) {
	url := CoverUrl(media)
	if url == "" {
		return nil, nil
	}
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed fetching cover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed fetching cover: status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed reading cover: %w", err)
	} else if len(data) > maxCoverSize {
		return nil, fmt.Errorf("cover image too large")
	}

	pic := &vorbis.Picture{Data: data, MimeType: http.DetectContentType(data)}
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		pic.Width, pic.Height = uint32(cfg.Width), uint32(cfg.Height)
		pic.MimeType = "image/" + format
	}
	return pic, nil
}

// WriteTaggedOgg writes the Ogg Vorbis audio in src (Spotify metadata page already
// removed) to w, with tags for media, ReplayGain values and the cover art embedded.
// A failure to fetch the cover is logged and the file is written without it.
func WriteTaggedOgg(ctx context.Context, log librespot.Logger, client *http.Client, w io.Writer, src io.ReaderAt, size int64, media *librespot.Media, rg *vorbis.ReplayGain) error {
	pic := fetchCoverLogged(ctx, log, client, media, 15*time.Second)
	return writeTaggedOgg(w, src, size, media, rg, pic, nil)
}

// fetchCoverLogged is FetchCover bounded by timeout; a failure is logged and
// gives nil, so the file is written without cover art.
func fetchCoverLogged(ctx context.Context, log librespot.Logger, client *http.Client, media *librespot.Media, timeout time.Duration) *vorbis.Picture {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pic, err := FetchCover(ctx, client, media)
	if err != nil {
		log.WithError(err).Warnf("writing ogg without cover art")
		return nil
	}
	return pic
}

func writeTaggedOgg(w io.Writer, src io.ReaderAt, size int64, media *librespot.Media, rg *vorbis.ReplayGain, pic *vorbis.Picture, onHeader func(srcHeader, dstHeader int64)) error {
	comments := VorbisComments(media, rg)
	if pic != nil {
		comments = append(comments, pic.MetadataBlockPicture())
	}
	return vorbis.WriteTaggedNotify(w, io.NewSectionReader(src, 0, size), comments, onHeader)
}

// WriteTagged writes the stream to w as a standalone Ogg Vorbis file with metadata,
// ReplayGain and cover art embedded. It reads the whole stream from the start.
func (s *EncodedStream) WriteTagged(ctx context.Context, log librespot.Logger, client *http.Client, w io.Writer) error {
	return WriteTaggedOgg(ctx, log, client, w, s.Source, s.Size, s.Media, s.ReplayGain)
}
