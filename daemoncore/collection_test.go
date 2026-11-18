//go:build test_unit

package daemoncore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestDecodeCollectionPage(t *testing.T) {
	var b []byte
	for _, it := range []collectionPageItem{
		{uri: "spotify:track:a", addedAt: 1700000000},
		{uri: "spotify:album:b", addedAt: 1600000000, isRemoved: true},
	} {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, encodeCollectionItem(it.uri, it.addedAt, it.isRemoved))
	}
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, "next")
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	b = protowire.AppendString(b, "sync")

	page, err := decodeCollectionPage(b)
	require.NoError(t, err)
	require.Equal(t, "next", page.nextToken)
	require.Equal(t, "sync", page.syncToken)
	require.Equal(t, []collectionPageItem{
		{uri: "spotify:track:a", addedAt: 1700000000},
		{uri: "spotify:album:b", addedAt: 1600000000, isRemoved: true},
	}, page.items)
}

func TestCollectionSetForURI(t *testing.T) {
	groups, err := groupBySet([]string{"spotify:track:a", "spotify:album:b", "spotify:artist:c", "spotify:track:a"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{
		"collection": {"spotify:track:a", "spotify:album:b"},
		"artist":     {"spotify:artist:c"},
	}, groups)

	_, err = groupBySet([]string{"spotify:playlist:x"})
	require.Error(t, err)
}

func TestSearchKindFromURI(t *testing.T) {
	require.Equal(t, "track", searchKindFromURI("spotify:track:abc"))
	require.Equal(t, "playlist", searchKindFromURI("spotify:user:me:playlist:abc"))
	require.Equal(t, "", searchKindFromURI("nope"))
}

func TestEncodeCollectionContainsRequest(t *testing.T) {
	b := encodeCollectionContainsRequest("user", "collection", []string{"spotify:track:a", "spotify:track:b"})
	fields := map[protowire.Number][]string{}
	require.NoError(t, walkProto(b, func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error {
		fields[num] = append(fields[num], string(v))
		return nil
	}))
	require.Equal(t, []string{"user"}, fields[1])
	require.Equal(t, []string{"collection"}, fields[2])
	var items []string
	for _, msg := range fields[3] {
		require.NoError(t, walkProto([]byte(msg), func(num protowire.Number, typ protowire.Type, v []byte, x uint64) error {
			if num == 1 {
				items = append(items, string(v))
			}
			return nil
		}))
	}
	require.Equal(t, []string{"spotify:track:a", "spotify:track:b"}, items)
}

func TestCleanDescription(t *testing.T) {
	require.Equal(t, "Shibuya Punk music for you. Also try Lofi Mix and Chill Mix",
		cleanDescription(`Shibuya Punk music for you. Also try <a href="spotify:playlist:1">Lofi Mix</a> and <a href="spotify:playlist:2">Chill Mix</a>`))
	require.Equal(t, "Rock & Roll's best", cleanDescription("Rock &amp; Roll&#x27;s best"))
	require.Equal(t, "plain", cleanDescription("  plain "))
}

func TestParseRootlistGroupStart(t *testing.T) {
	id, name, ok := parseRootlistGroupStart("spotify:start-group:1b1298123b003253:My+Lofi%21")
	require.True(t, ok)
	require.Equal(t, "1b1298123b003253", id)
	require.Equal(t, "My Lofi!", name)
	_, _, ok = parseRootlistGroupStart("spotify:end-group:1b1298123b003253")
	require.False(t, ok)
}
