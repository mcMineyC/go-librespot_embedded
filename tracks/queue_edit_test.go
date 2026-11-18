//go:build test_unit

package tracks

import (
	"context"
	"fmt"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/stretchr/testify/require"
)

func trackUri(i int) string {
	return fmt.Sprintf("spotify:track:%022d", i)
}

// newTestList builds a list over two in-memory pages of 3 tracks each, so no
// network is needed.
func newTestList(t *testing.T) *List {
	t.Helper()
	page := func(from int) *connectpb.ContextPage {
		p := &connectpb.ContextPage{}
		for i := from; i < from+3; i++ {
			p.Tracks = append(p.Tracks, &connectpb.ContextTrack{
				Uri: trackUri(i), Uid: fmt.Sprintf("u%d", i), Metadata: map[string]string{"title": fmt.Sprintf("T%d", i)},
			})
		}
		return p
	}
	tl, err := NewTrackListFromContext(context.Background(), &librespot.NullLogger{}, nil, &connectpb.Context{
		Uri:   "spotify:internal:test",
		Pages: []*connectpb.ContextPage{page(0), page(3)},
	})
	require.NoError(t, err)
	require.NoError(t, tl.TrySeek(context.Background(), func(ct *connectpb.ContextTrack) bool { return ct.Uri == trackUri(1) }))
	return tl
}

func nextUris(tl *List) []string {
	var out []string
	for _, t := range tl.NextTracks(context.Background(), nil) {
		out = append(out, t.Uri)
	}
	return out
}

func TestReplaceUpcomingReordersAndKeepsMetadata(t *testing.T) {
	tl := newTestList(t)
	tl.AddToQueue(&connectpb.ContextTrack{Uri: trackUri(9), Uid: "q1"})

	want := []string{trackUri(5), trackUri(9), trackUri(2), trackUri(7)}
	require.NoError(t, tl.ReplaceUpcoming(context.Background(), want))

	require.Equal(t, trackUri(1), tl.CurrentTrack().Uri)
	require.Equal(t, want, nextUris(tl))

	next := tl.NextTracks(context.Background(), nil)
	require.Equal(t, "u5", next[0].Uid, "existing entries keep their uid")
	require.Equal(t, "T5", next[0].Metadata["title"], "existing entries keep their metadata")
	require.NotEqual(t, "true", next[1].Metadata["is_queued"], "queued entries become context entries")
	require.NotEmpty(t, next[3].Uid, "new entries get a uid")

	// Walking forward follows the new order and then ends.
	for _, uri := range want {
		require.True(t, tl.GoNext(context.Background()))
		require.Equal(t, uri, tl.CurrentTrack().Uri)
	}
	require.False(t, tl.GoNext(context.Background()))

	// History is untouched.
	require.True(t, tl.GoPrev())
	require.Equal(t, trackUri(2), tl.CurrentTrack().Uri)
}

func TestReplaceUpcomingWhilePlayingQueueItem(t *testing.T) {
	tl := newTestList(t)
	tl.AddToQueue(&connectpb.ContextTrack{Uri: trackUri(9), Uid: "q1"})
	require.True(t, tl.GoNext(context.Background()))
	require.Equal(t, trackUri(9), tl.CurrentTrack().Uri)

	require.NoError(t, tl.ReplaceUpcoming(context.Background(), []string{trackUri(4)}))
	require.Equal(t, trackUri(9), tl.CurrentTrack().Uri)
	require.Equal(t, []string{trackUri(4)}, nextUris(tl))

	require.True(t, tl.GoNext(context.Background()))
	require.Equal(t, trackUri(4), tl.CurrentTrack().Uri)
}

func TestInsertIntoQueue(t *testing.T) {
	tl := newTestList(t)
	tl.AddToQueue(&connectpb.ContextTrack{Uri: trackUri(10)})
	tl.InsertIntoQueue(0, &connectpb.ContextTrack{Uri: trackUri(11)})
	tl.InsertIntoQueue(99, &connectpb.ContextTrack{Uri: trackUri(12)})
	tl.InsertIntoQueue(1, &connectpb.ContextTrack{Uri: trackUri(13)})

	require.Equal(t, []string{trackUri(11), trackUri(13), trackUri(10), trackUri(12), trackUri(2), trackUri(3), trackUri(4), trackUri(5)}, nextUris(tl))

	// While a queue item plays, index 0 still means "next".
	require.True(t, tl.GoNext(context.Background()))
	require.Equal(t, trackUri(11), tl.CurrentTrack().Uri)
	tl.InsertIntoQueue(0, &connectpb.ContextTrack{Uri: trackUri(14)})
	require.Equal(t, trackUri(14), nextUris(tl)[0])
	require.Equal(t, trackUri(11), tl.CurrentTrack().Uri)
}

// Clearing the queue (SetQueue with a nil next list) must drop only the queued
// tracks and keep the rest of the context upcoming.
func TestClearQueueKeepsContext(t *testing.T) {
	tl := newTestList(t)
	tl.AddToQueue(&connectpb.ContextTrack{Uri: trackUri(9), Uid: "q1"})
	require.Equal(t, trackUri(9), nextUris(tl)[0])

	tl.SetQueue(nil, nil)
	require.Equal(t, []string{trackUri(2), trackUri(3), trackUri(4), trackUri(5)}, nextUris(tl))
}
