package daemoncore

import (
	"context"
	"fmt"
	"io"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/player"
)

// waitForCountryCode blocks until the access point has sent the account's country
// code, which is needed to check track restrictions. The player must be running.
func (p *AppPlayer) waitForCountryCode(ctx context.Context) error {
	for *p.countryCode == "" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for country code: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// NewEncodedStream resolves uri (spotify:track:… or spotify:episode:…) to a
// decrypted, standard Ogg Vorbis stream without decoding it. The player must
// have been started with App.StartPlayer.
func (p *AppPlayer) NewEncodedStream(ctx context.Context, uri string) (*player.EncodedStream, error) {
	id, err := librespot.SpotifyIdFromUri(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid uri: %w", err)
	}
	if err := p.waitForCountryCode(ctx); err != nil {
		return nil, err
	}
	return p.player.NewEncodedStream(ctx, p.app.client, *id, p.app.cfg.Bitrate, 0)
}

// WriteTaggedOgg writes st to w as a complete Ogg Vorbis file with metadata,
// ReplayGain and cover art embedded.
func (p *AppPlayer) WriteTaggedOgg(ctx context.Context, st *player.EncodedStream, w io.Writer) error {
	return st.WriteTagged(ctx, p.app.log, p.app.client, w)
}
