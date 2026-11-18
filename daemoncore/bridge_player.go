package daemoncore

import (
	"context"
	"fmt"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

// ---------------------------------------------------------------------------
// Run-loop execution
// ---------------------------------------------------------------------------

// playerCall is a function sent from another goroutine to run on the Run loop.
type playerCall struct {
	fn   func(ctx context.Context)
	done chan struct{}
}

// exec runs fn on the Run loop and waits for it to finish. Player state is
// owned by the Run loop, so every exported Api* method goes through here
// instead of touching state from the caller's goroutine. Callers therefore
// need no lock of their own, and slow network work elsewhere (metadata,
// search, library) never blocks playback controls.
//
// If the Run loop is not running (not started yet, or stopped), fn runs on the
// calling goroutine.
func (p *AppPlayer) exec(fn func(ctx context.Context)) {
	if p.calls == nil || !p.running.Load() {
		fn(context.Background())
		return
	}

	c := playerCall{fn: fn, done: make(chan struct{})}
	select {
	case p.calls <- c:
		<-c.done
	case <-p.runDone:
		fn(context.Background())
	}
}

func execValue[T any](p *AppPlayer, fn func(ctx context.Context) (T, error)) (out T, err error) {
	p.exec(func(ctx context.Context) { out, err = fn(ctx) })
	return out, err
}

func (p *AppPlayer) execErr(fn func(ctx context.Context) error) (err error) {
	p.exec(func(ctx context.Context) { err = fn(ctx) })
	return err
}

func (p *AppPlayer) runCall(ctx context.Context, c playerCall) {
	defer close(c.done)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c.fn(ctx)
}

func (p *AppPlayer) markRunDone() {
	p.running.Store(false)
	p.runDoneOnce.Do(func() {
		if p.runDone != nil {
			close(p.runDone)
		}
	})
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func (p *AppPlayer) emitError(source string, err error) {
	if err == nil {
		return
	}
	data := ApiEventDataError{Source: source, Message: err.Error()}
	if p.state != nil && p.state.player != nil && p.state.player.Track != nil {
		data.Uri = p.state.player.Track.Uri
	}
	p.app.server.Emit(&ApiEvent{Type: ApiEventTypeError, Data: data})
}

// maybeEmitQueueChanged emits queue_changed when the context, the current
// track or the upcoming tracks differ from the last announcement.
func (p *AppPlayer) maybeEmitQueueChanged() {
	if p.state == nil || p.state.player == nil {
		return
	}
	st := p.state.player

	var b strings.Builder
	b.WriteString(st.ContextUri)
	b.WriteByte('|')
	if st.Track != nil {
		b.WriteString(st.Track.Uid)
		b.WriteByte('/')
		b.WriteString(st.Track.Uri)
	}
	b.WriteByte('|')
	for _, t := range st.NextTracks {
		b.WriteString(t.Uid)
		b.WriteByte('/')
		b.WriteString(t.Uri)
		b.WriteByte(',')
	}
	key := b.String()
	if key == p.lastQueueKey {
		return
	}
	p.lastQueueKey = key

	data := ApiEventDataQueueChanged{ContextUri: st.ContextUri, NextCount: len(st.NextTracks)}
	if st.Track != nil {
		data.Uri = st.Track.Uri
	}
	p.app.server.Emit(&ApiEvent{Type: ApiEventTypeQueueChanged, Data: data})
}

// ---------------------------------------------------------------------------
// Basic controls
// ---------------------------------------------------------------------------

func (p *AppPlayer) apiRequest(req ApiRequest) (any, error) {
	return execValue(p, func(ctx context.Context) (any, error) { return p.handleApiRequest(ctx, req) })
}

func (p *AppPlayer) ApiPlay() error {
	_, err := p.apiRequest(ApiRequest{Type: ApiRequestTypeResume})
	return err
}

func (p *AppPlayer) ApiPause() error {
	_, err := p.apiRequest(ApiRequest{Type: ApiRequestTypePause})
	return err
}

func (p *AppPlayer) ApiNext(uri string) error {
	var data ApiRequestDataNext
	if uri != "" {
		data.Uri = &uri
	}
	_, err := p.apiRequest(ApiRequest{Type: ApiRequestTypeNext, Data: data})
	return err
}

func (p *AppPlayer) ApiPrev() error {
	_, err := p.apiRequest(ApiRequest{Type: ApiRequestTypePrev})
	return err
}

func (p *AppPlayer) ApiPrevTo(uri string) error {
	if uri == "" {
		return p.ApiPrev()
	}
	_, err := p.apiRequest(ApiRequest{Type: ApiRequestTypePrevTo, Data: ApiRequestDataNext{Uri: &uri}})
	return err
}

// ApiSeek seeks the current track to positionMs milliseconds from the start.
func (p *AppPlayer) ApiSeek(positionMs int64) error {
	_, err := p.apiRequest(ApiRequest{
		Type: ApiRequestTypeSeek,
		Data: ApiRequestDataSeek{Position: positionMs, Relative: false},
	})
	return err
}

// ApiSetRepeat sets the repeat mode: "off", "context" or "track".
func (p *AppPlayer) ApiSetRepeat(mode string) error {
	var wantContext, wantTrack bool
	switch mode {
	case "off":
	case "context":
		wantContext = true
	case "track":
		wantContext, wantTrack = true, true
	default:
		return fmt.Errorf("invalid repeat mode %q: must be off, context, or track", mode)
	}
	return p.execErr(func(ctx context.Context) error {
		p.setOptions(ctx, &wantContext, &wantTrack, nil)
		return nil
	})
}

// ApiSetShuffle enables or disables shuffle.
func (p *AppPlayer) ApiSetShuffle(enabled bool) error {
	_, err := p.apiRequest(ApiRequest{Type: ApiRequestTypeSetShufflingContext, Data: enabled})
	return err
}

// ApiSetVolume sets the volume to vol steps out of the configured VolumeSteps.
func (p *AppPlayer) ApiSetVolume(vol int32) error {
	_, err := p.apiRequest(ApiRequest{
		Type: ApiRequestTypeSetVolume,
		Data: ApiRequestDataVolume{Volume: vol, Relative: false},
	})
	return err
}

// ApiGetVolume returns the current volume (0..VolumeSteps) and VolumeSteps.
func (p *AppPlayer) ApiGetVolume() (value uint32, max uint32) {
	p.exec(func(context.Context) { value, max = p.apiVolume(), p.app.cfg.VolumeSteps })
	return value, max
}

// ApiSetLocalGain sets a 0..1 gain applied on this device only. It is meant
// for ducking and fades: the Spotify Connect volume other devices see does
// not change.
func (p *AppPlayer) ApiSetLocalGain(gain float32) {
	p.player.SetLocalGain(gain)
}

// ApiPlayUri resolves and loads a Spotify context URI (playlist, album, artist,
// track, episode, show, collection). skipToUri, if non-empty, skips within the
// context to that track URI. paused controls whether playback starts paused.
func (p *AppPlayer) ApiPlayUri(uri, skipToUri string, paused bool) error {
	_, err := p.apiRequest(ApiRequest{
		Type: ApiRequestTypePlay,
		Data: ApiRequestDataPlay{Uri: uri, SkipToUri: skipToUri, Paused: paused},
	})
	return err
}

// BridgeTracksContextUri is the context URI used for track lists started with
// ApiPlayTracks.
const BridgeTracksContextUri = "spotify:internal:tracks"

// ApiPlayTracks plays an explicit list of track/episode URIs as one context,
// starting at startIndex and positionMs. The list behaves like any other
// context: next/previous, shuffle, repeat and the queue all work on it.
func (p *AppPlayer) ApiPlayTracks(uris []string, startIndex int, positionMs int64, paused bool) error {
	if len(uris) == 0 {
		return fmt.Errorf("no tracks given")
	}
	if startIndex < 0 || startIndex >= len(uris) {
		return fmt.Errorf("start index %d out of range (0..%d)", startIndex, len(uris)-1)
	}

	ctxTracks := make([]*connectpb.ContextTrack, 0, len(uris))
	for i, uri := range uris {
		id, err := librespot.SpotifyIdFromUri(uri)
		if err != nil {
			return fmt.Errorf("invalid uri at index %d: %w", i, err)
		} else if id.Type() != librespot.SpotifyIdTypeTrack && id.Type() != librespot.SpotifyIdTypeEpisode {
			return fmt.Errorf("uri at index %d is not a track or episode: %s", i, uri)
		}
		ctxTracks = append(ctxTracks, &connectpb.ContextTrack{
			Uri:      uri,
			Uid:      fmt.Sprintf("t%d", i),
			Gid:      id.Id(),
			Metadata: map[string]string{},
		})
	}
	startUid := ctxTracks[startIndex].Uid

	return p.execErr(func(ctx context.Context) error {
		spotCtx := &connectpb.Context{
			Uri:      BridgeTracksContextUri,
			Url:      "context://" + BridgeTracksContextUri,
			Metadata: map[string]string{},
			Pages:    []*connectpb.ContextPage{{Tracks: ctxTracks}},
		}

		p.state.setActive(true)
		p.state.setPaused(paused)
		p.state.player.Suppressions = &connectpb.Suppressions{}
		p.state.player.PlayOrigin = &connectpb.PlayOrigin{
			FeatureIdentifier: "go-librespot",
			FeatureVersion:    librespot.VersionNumberString(),
		}

		skipTo := func(t *connectpb.ContextTrack) bool { return t.Uid == startUid }
		if err := p.loadContextAt(ctx, spotCtx, skipTo, paused, true, positionMs); err != nil {
			p.emitError("load_context", err)
			return fmt.Errorf("failed loading tracks: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// PlayerStatus is a snapshot of the playback state.
type PlayerStatus struct {
	Active     bool   `json:"active"`
	Playing    bool   `json:"playing"` // a track is loaded and not paused
	Paused     bool   `json:"paused"`
	Buffering  bool   `json:"buffering"`
	Stopped    bool   `json:"stopped"` // nothing loaded
	ContextUri string `json:"context_uri,omitempty"`
	TrackUri   string `json:"track_uri,omitempty"`
	TrackUid   string `json:"track_uid,omitempty"`
	// PositionMs is the playback position as of TimestampMs (unix ms). While
	// playing, add the time elapsed since TimestampMs to extrapolate.
	PositionMs    int64   `json:"position_ms"`
	TimestampMs   int64   `json:"timestamp_ms"`
	DurationMs    int64   `json:"duration_ms"`
	Shuffle       bool    `json:"shuffle"`
	RepeatContext bool    `json:"repeat_context"`
	RepeatTrack   bool    `json:"repeat_track"`
	RepeatMode    string  `json:"repeat_mode"` // off | context | track
	Volume        uint32  `json:"volume"`
	VolumeMax     uint32  `json:"volume_max"`
	LocalGain     float32 `json:"local_gain"`
}

// ApiGetStatus returns the current playback state, including the exact
// position reported by the audio pipeline.
func (p *AppPlayer) ApiGetStatus() *PlayerStatus {
	var out *PlayerStatus
	p.exec(func(context.Context) {
		st := p.state.player
		out = &PlayerStatus{
			Active:        p.state.active,
			Paused:        st.IsPaused,
			Buffering:     st.IsBuffering,
			Stopped:       !st.IsPlaying,
			ContextUri:    st.ContextUri,
			Shuffle:       st.Options.GetShufflingContext(),
			RepeatContext: st.Options.GetRepeatingContext(),
			RepeatTrack:   st.Options.GetRepeatingTrack(),
			Volume:        p.apiVolume(),
			VolumeMax:     p.app.cfg.VolumeSteps,
			LocalGain:     p.player.LocalGain(),
		}
		out.Playing = st.IsPlaying && !st.IsPaused
		switch {
		case out.RepeatTrack:
			out.RepeatMode = "track"
		case out.RepeatContext:
			out.RepeatMode = "context"
		default:
			out.RepeatMode = "off"
		}
		if st.Track != nil {
			out.TrackUri = st.Track.Uri
			out.TrackUid = st.Track.Uid
		}
		if p.primaryStream != nil {
			out.DurationMs = int64(p.primaryStream.Media.Duration())
			out.PositionMs = p.player.PositionMs()
		} else {
			out.DurationMs = st.Duration
			out.PositionMs = p.state.trackPosition()
		}
		out.TimestampMs = time.Now().UnixMilli()
	})
	return out
}

// ---------------------------------------------------------------------------
// Queue
// ---------------------------------------------------------------------------

// ApiAddToQueue appends a Spotify track or episode URI to the playback queue.
func (p *AppPlayer) ApiAddToQueue(uri string) error {
	_, err := p.apiRequest(ApiRequest{Type: ApiRequestTypeAddToQueue, Data: uri})
	return err
}

// ApiInsertIntoQueue inserts a track or episode into the user queue at index
// (0 plays next). Out of range indexes are clamped.
func (p *AppPlayer) ApiInsertIntoQueue(uri string, index int) error {
	if _, err := librespot.SpotifyIdFromUri(uri); err != nil {
		return err
	}
	return p.execErr(func(ctx context.Context) error {
		if p.state.tracks == nil {
			return fmt.Errorf("no active context")
		}
		p.state.queueID++
		p.state.tracks.InsertIntoQueue(index, &connectpb.ContextTrack{
			Uri: uri,
			Uid: fmt.Sprintf("q%d", p.state.queueID),
		})
		p.refreshQueueState(ctx)
		return nil
	})
}

// ApiSetUpcoming replaces everything after the current track (user queue and
// the rest of the context) with uris, in that order. This is how a host
// reorders, removes or inserts upcoming tracks. Shuffle is turned off, since
// the order is now explicit. The current track keeps playing.
func (p *AppPlayer) ApiSetUpcoming(uris []string) error {
	for i, uri := range uris {
		if _, err := librespot.SpotifyIdFromUri(uri); err != nil {
			return fmt.Errorf("invalid uri at index %d: %w", i, err)
		}
	}
	return p.execErr(func(ctx context.Context) error {
		if p.state.tracks == nil {
			return fmt.Errorf("no active context")
		}
		if err := p.state.tracks.ReplaceUpcoming(ctx, uris); err != nil {
			return err
		}
		if p.state.player.Options.ShufflingContext {
			p.state.player.Options.ShufflingContext = false
			p.app.server.Emit(&ApiEvent{Type: ApiEventTypeShuffleContext, Data: ApiEventDataShuffleContext{Value: false}})
		}
		p.refreshQueueState(ctx)
		return nil
	})
}

func (p *AppPlayer) refreshQueueState(ctx context.Context) {
	p.state.player.PrevTracks = p.state.tracks.PrevTracks()
	p.state.player.NextTracks = p.state.tracks.NextTracks(ctx, nil)
	p.state.player.Index = p.state.tracks.Index()
	p.updateState(ctx)
	p.schedulePrefetchNext()
}

// ApiRemoveFromQueue removes the first occurrence of the given URI from the
// play-ahead (next) queue and updates the player state. It does not affect the
// current track or the history (prev) tracks.
func (p *AppPlayer) ApiRemoveFromQueue(uri string) error {
	return p.execErr(func(ctx context.Context) error { return p.removeFromQueue(ctx, uri) })
}

func (p *AppPlayer) removeFromQueue(ctx context.Context, uri string) error {
	if p.state == nil || p.state.tracks == nil {
		return fmt.Errorf("no active context")
	}

	next := p.state.player.NextTracks
	filtered := make([]*connectpb.ContextTrack, 0, len(next))
	removed := false
	for _, t := range next {
		if !removed && t.Uri == uri {
			removed = true
			continue
		}
		filtered = append(filtered, librespot.ProvidedTrackToContextTrack(t))
	}
	if !removed {
		return fmt.Errorf("uri %q not found in queue", uri)
	}
	prevCT := make([]*connectpb.ContextTrack, 0, len(p.state.player.PrevTracks))
	for _, t := range p.state.player.PrevTracks {
		prevCT = append(prevCT, librespot.ProvidedTrackToContextTrack(t))
	}
	p.setQueue(ctx, prevCT, filtered)
	return nil
}

// QueueInfo holds a snapshot of the playback queue.
type QueueInfo struct {
	Current *TrackSummary
	Prev    []TrackSummary
	Next    []TrackSummary
}

// TrackSummary is a queue entry as known by the player, without network
// lookups. Provider is "context", "queue" or "autoplay".
type TrackSummary struct {
	URI      string
	Uid      string
	Name     string
	Provider string
}

func summaryFromProvided(t *connectpb.ProvidedTrack) TrackSummary {
	return TrackSummary{URI: t.Uri, Uid: t.Uid, Name: t.Metadata["title"], Provider: t.Provider}
}

// ApiGetQueue returns the current playback queue as QueueInfo.
func (p *AppPlayer) ApiGetQueue() *QueueInfo {
	var out *QueueInfo
	p.exec(func(context.Context) { out = p.getQueue() })
	return out
}

func (p *AppPlayer) getQueue() *QueueInfo {
	if p.state == nil || p.state.player == nil {
		return &QueueInfo{}
	}

	out := &QueueInfo{
		Prev: make([]TrackSummary, 0, len(p.state.player.PrevTracks)),
		Next: make([]TrackSummary, 0, len(p.state.player.NextTracks)),
	}
	if p.state.player.Track != nil {
		cur := summaryFromProvided(p.state.player.Track)
		out.Current = &cur
	}
	for _, t := range p.state.player.PrevTracks {
		out.Prev = append(out.Prev, summaryFromProvided(t))
	}
	for _, t := range p.state.player.NextTracks {
		out.Next = append(out.Next, summaryFromProvided(t))
	}
	return out
}

// QueueEntry is a queue item with display metadata resolved.
type QueueEntry struct {
	TrackDetails
	Uid      string `json:"uid,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// DetailedQueue is a queue snapshot with metadata for every entry.
type DetailedQueue struct {
	ContextUri string       `json:"context_uri,omitempty"`
	Current    *QueueEntry  `json:"current"`
	Prev       []QueueEntry `json:"prev"`
	Next       []QueueEntry `json:"next"`
}

// GetDetailedQueue snapshots the queue on the Run loop, then resolves artist,
// album, image and duration for each entry with one batched (and cached)
// metadata request. If metadata cannot be fetched the entries still carry
// uri, name, uid and provider.
func (p *AppPlayer) GetDetailedQueue(ctx context.Context) *DetailedQueue {
	var q *QueueInfo
	var contextUri string
	p.exec(func(context.Context) {
		q = p.getQueue()
		if p.state != nil && p.state.player != nil {
			contextUri = p.state.player.ContextUri
		}
	})

	uris := make([]string, 0, len(q.Prev)+len(q.Next)+1)
	if q.Current != nil {
		uris = append(uris, q.Current.URI)
	}
	for _, t := range q.Prev {
		uris = append(uris, t.URI)
	}
	for _, t := range q.Next {
		uris = append(uris, t.URI)
	}

	details := map[string]TrackDetails{}
	if list, err := p.app.GetTracksDetails(ctx, p, uris); err == nil {
		for _, d := range list {
			details[d.URI] = d
		}
	} else {
		p.app.log.WithError(err).Warn("failed resolving queue metadata")
	}

	entry := func(t TrackSummary) QueueEntry {
		d, ok := details[t.URI]
		if !ok {
			d = TrackDetails{URI: t.URI, Name: t.Name, Artists: []ArtistRef{}}
		}
		return QueueEntry{TrackDetails: d, Uid: t.Uid, Provider: t.Provider}
	}

	out := &DetailedQueue{ContextUri: contextUri, Prev: []QueueEntry{}, Next: []QueueEntry{}}
	if q.Current != nil {
		e := entry(*q.Current)
		out.Current = &e
	}
	for _, t := range q.Prev {
		out.Prev = append(out.Prev, entry(t))
	}
	for _, t := range q.Next {
		out.Next = append(out.Next, entry(t))
	}
	return out
}

// ApiClearQueue removes all user-queued tracks from the next-tracks list,
// keeping only context tracks.
func (p *AppPlayer) ApiClearQueue(ctx context.Context) {
	p.exec(func(execCtx context.Context) {
		if p.state == nil || p.state.tracks == nil {
			return
		}
		prevCT := make([]*connectpb.ContextTrack, 0, len(p.state.player.PrevTracks))
		for _, t := range p.state.player.PrevTracks {
			prevCT = append(prevCT, librespot.ProvidedTrackToContextTrack(t))
		}
		// A nil next list drops the user queue and rebuilds the upcoming
		// tracks from the context; an empty one would mean "nothing next".
		p.setQueue(execCtx, prevCT, nil)
	})
}

// SurroundingTracks returns up to n tracks from PrevTracks and NextTracks on
// the current player state. prev is ordered oldest-first; next is in play order.
func (p *AppPlayer) SurroundingTracks(n int) (prev []TrackSummary, next []TrackSummary) {
	p.exec(func(context.Context) {
		if p.state == nil || p.state.player == nil {
			return
		}
		rawPrev := p.state.player.PrevTracks
		if start := len(rawPrev) - n; start > 0 {
			rawPrev = rawPrev[start:]
		}
		for _, t := range rawPrev {
			prev = append(prev, summaryFromProvided(t))
		}
		rawNext := p.state.player.NextTracks
		if n < len(rawNext) {
			rawNext = rawNext[:n]
		}
		for _, t := range rawNext {
			next = append(next, summaryFromProvided(t))
		}
	})
	return prev, next
}

// ---------------------------------------------------------------------------
// Devices
// ---------------------------------------------------------------------------

// ApiListDevices returns all Spotify Connect devices visible in the current
// cluster state, without a network round-trip.
func (p *AppPlayer) ApiListDevices() []DeviceInfo {
	var out []DeviceInfo
	p.exec(func(context.Context) { out = p.listDevices() })
	return out
}

func (p *AppPlayer) listDevices() []DeviceInfo {
	if p.state == nil {
		return nil
	}
	return p.listDevicesFromCluster()
}

// ApiTransferPlayback asks the Spotify backend to move active playback to
// deviceId. If play is true, playback starts immediately on the target.
func (p *AppPlayer) ApiTransferPlayback(ctx context.Context, deviceId string, play bool) error {
	if deviceId == "" {
		return fmt.Errorf("deviceId must not be empty")
	}
	return p.transferPlayback(ctx, deviceId, play)
}
