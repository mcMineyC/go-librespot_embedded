package player

import (
	"bytes"
	"io"

	librespot "github.com/devgianlu/go-librespot"
	audiofilespb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata/audiofiles"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	"github.com/devgianlu/go-librespot/vorbis"
)

type Stream struct {
	PlaybackId []byte

	Source librespot.AudioSource
	Media  *librespot.Media
	File   *metadatapb.AudioFile
	Size   int64
}

type EncodedStream struct {
	PlaybackId []byte

	// Source is a standard Ogg Vorbis stream; Spotify's metadata page has been removed.
	Source io.ReaderAt
	Media  *librespot.Media
	File   *metadatapb.AudioFile
	Size   int64

	// ReplayGain holds the values from Spotify's in-file metadata page, nil if absent.
	ReplayGain *vorbis.ReplayGain
	// TrackNormalization and AlbumNormalization are the loudness values (LUFS and dBTP)
	// the player uses for normalisation. They are nil for episodes.
	TrackNormalization *audiofilespb.NormalizationParams
	AlbumNormalization *audiofilespb.NormalizationParams
}

func (s *Stream) Is(id librespot.SpotifyId) bool {
	if id.Type() == librespot.SpotifyIdTypeTrack && s.Media.IsTrack() {
		return bytes.Equal(id.Id(), s.Media.Track().Gid)
	} else if id.Type() == librespot.SpotifyIdTypeEpisode && s.Media.IsEpisode() {
		return bytes.Equal(id.Id(), s.Media.Episode().Gid)
	} else {
		return false
	}
}
