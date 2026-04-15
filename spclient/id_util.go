package spclient

import (
	"errors"
	"math/big"
	"strings"
)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

type SpotifyIDType string

const (
	TypeTrack    SpotifyIDType = "track"
	TypeAlbum    SpotifyIDType = "album"
	TypeArtist   SpotifyIDType = "artist"
	TypePlaylist SpotifyIDType = "playlist"
)

// NormalizeSpotifyURI extracts type + base62 ID from any input
func NormalizeSpotifyURI(input string) (SpotifyIDType, string, error) {
	if strings.HasPrefix(input, "spotify:") {
		parts := strings.Split(input, ":")
		if len(parts) < 3 {
			return "", "", errors.New("invalid spotify URI")
		}
		return SpotifyIDType(parts[1]), parts[2], nil
	}

	if strings.Contains(input, "open.spotify.com") {
		parts := strings.Split(input, "/")
		if len(parts) < 5 {
			return "", "", errors.New("invalid spotify URL")
		}
		id := parts[len(parts)-1]
		id = strings.Split(id, "?")[0]
		return SpotifyIDType(parts[len(parts)-2]), id, nil
	}

	// fallback: assume raw ID (unknown type)
	return "", input, nil
}

// Base62 → hex GID
func Base62ToHex(id string) (string, error) {
	result := big.NewInt(0)
	base := big.NewInt(62)

	for _, c := range id {
		index := int64(strings.IndexRune(base62Alphabet, c))
		if index < 0 {
			return "", errors.New("invalid base62 character")
		}
		result.Mul(result, base)
		result.Add(result, big.NewInt(index))
	}

	hex := result.Text(16)

	// pad to 16 bytes (32 hex chars)
	for len(hex) < 32 {
		hex = "0" + hex
	}

	return hex, nil
}