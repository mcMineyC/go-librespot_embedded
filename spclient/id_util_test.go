package spclient

import (
	"encoding/hex"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

func TestBase62ToHexMatchesSpotifyId(t *testing.T) {
	for _, id := range []string{
		"5W3cjX2J3tjhG8zb6u0qHn", // track
		"2noRn2Aes5aoNVsU6iWThc", // album
		"4tZwfgrHOc3mvqYlEYSvVi", // artist
		"0000000000000000000000",
	} {
		want, err := librespot.SpotifyIdFromBase62(librespot.SpotifyIdTypeTrack, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		got, err := Base62ToHex(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != hex.EncodeToString(want.Id()) {
			t.Errorf("Base62ToHex(%s) = %s, want %s", id, got, hex.EncodeToString(want.Id()))
		}
	}
}
