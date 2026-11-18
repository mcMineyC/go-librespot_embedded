// mobile_host is a terminal host for the mobilebind bridge. It exercises the
// same API an Android/iOS app uses, through an interactive shell.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
	"github.com/devgianlu/go-librespot/mobilebind"
)

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

// out is where everything is printed. Once the shell starts it is the
// readline writer, which redraws the prompt after asynchronous output (events
// and logs) instead of printing over what the user is typing.
var (
	outMu sync.RWMutex
	out   io.Writer = os.Stdout
)

func printf(format string, args ...any) {
	outMu.RLock()
	w := out
	outMu.RUnlock()
	_, _ = fmt.Fprintf(w, format, args...)
}

func println(args ...any) { printf("%s", fmt.Sprintln(args...)) }

func printJSON(raw string) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		println(raw)
		return
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	println(string(b))
}

func fmtDuration(ms int64) string {
	if ms <= 0 {
		return "-:--"
	}
	s := ms / 1000
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// fmtPosition formats a playback position; unlike fmtDuration, 0 is "0:00".
func fmtPosition(ms int64) string {
	if ms < 0 {
		return "-:--"
	}
	s := ms / 1000
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Callbacks
// ---------------------------------------------------------------------------

type eventMode int

const (
	eventsCompact eventMode = iota
	eventsFull
	eventsOff
)

type host struct {
	credentialsPath string

	mu          sync.Mutex
	currentName string
	events      eventMode
	logLevel    string // minimum level shown: debug|info|warn|error
}

func (h *host) OnEvent(event string, payloadJson string) {
	h.mu.Lock()
	if event == "will_play" {
		h.currentName = ""
	}
	if event == "metadata" {
		var meta struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(payloadJson), &meta) == nil && meta.Name != "" {
			h.currentName = meta.Name
		}
	}
	mode := h.events
	h.mu.Unlock()

	switch mode {
	case eventsOff:
	case eventsFull:
		printf("[event] %s %s\n", event, payloadJson)
	default:
		printf("[event] %s %s\n", event, summarizeEvent(event, payloadJson))
	}
}

// summarizeEvent turns an event payload into a short, readable line.
func summarizeEvent(event, payload string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(payload), &m)
	str := func(k string) string { s, _ := m[k].(string); return s }
	switch event {
	case "metadata":
		artists := ""
		if a, ok := m["artist_names"].([]any); ok && len(a) > 0 {
			parts := make([]string, 0, len(a))
			for _, x := range a {
				parts = append(parts, fmt.Sprint(x))
			}
			artists = " — " + strings.Join(parts, ", ")
		}
		dur, _ := m["duration"].(float64)
		return fmt.Sprintf("%s%s (%s)", str("name"), artists, fmtDuration(int64(dur)))
	case "playing", "paused", "will_play", "not_playing":
		return str("uri")
	case "queue_changed":
		n, _ := m["next_count"].(float64)
		return fmt.Sprintf("%d upcoming", int(n))
	case "error":
		return fmt.Sprintf("%s: %s", str("source"), str("message"))
	case "seek":
		pos, _ := m["position"].(float64)
		return fmtPosition(int64(pos))
	case "volume":
		v, _ := m["value"].(float64)
		mx, _ := m["max"].(float64)
		return fmt.Sprintf("%d/%d", int(v), int(mx))
	case "repeat_track", "repeat_context", "shuffle_context":
		return fmt.Sprint(m["value"])
	}
	if payload == "null" || payload == "{}" {
		return ""
	}
	return truncate(payload, 120)
}

var logLevels = map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "warning": 3, "error": 4}

func (h *host) OnLog(level string, message string) {
	h.mu.Lock()
	min := h.logLevel
	h.mu.Unlock()
	if logLevels[strings.ToLower(level)] < logLevels[min] {
		return
	}
	printf("[log] %s: %s\n", level, message)
}

func (h *host) OnCredentialsPersist(credentialsJson string) error {
	if h.credentialsPath == "" {
		return fmt.Errorf("credentials path not set")
	}
	if err := os.MkdirAll(filepath.Dir(h.credentialsPath), 0700); err != nil {
		return fmt.Errorf("failed creating credentials directory: %w", err)
	}
	if err := os.WriteFile(h.credentialsPath, []byte(credentialsJson), 0600); err != nil {
		return fmt.Errorf("failed writing credentials: %w", err)
	}
	printf("Credentials saved to %s\n", h.credentialsPath)
	return nil
}

func (h *host) current() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.currentName
}

// ---------------------------------------------------------------------------
// Shell
// ---------------------------------------------------------------------------

type shell struct {
	lib *mobilebind.Librespot
	h   *host
	rl  *readline.Instance
}

type command struct {
	names []string
	usage string
	help  string
	run   func(s *shell, args []string) error
}

var errUsage = errors.New("usage")

// ask reads one line with a temporary prompt. An empty answer, Ctrl+C or
// Ctrl+D returns "".
func (s *shell) ask(prompt string) string {
	old := s.rl.Config.Prompt
	s.rl.SetPrompt(prompt)
	defer s.rl.SetPrompt(old)
	s.rl.Config.DisableAutoSaveHistory = true
	defer func() { s.rl.Config.DisableAutoSaveHistory = false }()
	line, err := s.rl.Readline()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(line)
}

func (s *shell) confirm(question string) bool {
	a := strings.ToLower(s.ask(question + " [y/N] "))
	return a == "y" || a == "yes"
}

// pick shows a numbered list and returns the chosen index, or -1 if
// cancelled. Typing text instead of a number filters the list.
func (s *shell) pick(title string, labels []string) int {
	if len(labels) == 0 {
		println("(nothing to choose from)")
		return -1
	}
	visible := make([]int, len(labels))
	for i := range labels {
		visible[i] = i
	}
	for {
		println(title)
		const max = 40
		for n, i := range visible {
			if n == max {
				printf("  … %d more; type text to filter\n", len(visible)-max)
				break
			}
			printf("  %3d  %s\n", i+1, labels[i])
		}
		answer := s.ask("number or filter (enter to cancel)> ")
		if answer == "" {
			return -1
		}
		if n, err := strconv.Atoi(answer); err == nil {
			if n >= 1 && n <= len(labels) {
				return n - 1
			}
			printf("%d is not in the list\n", n)
			continue
		}
		needle := strings.ToLower(answer)
		var filtered []int
		for i, l := range labels {
			if strings.Contains(strings.ToLower(l), needle) {
				filtered = append(filtered, i)
			}
		}
		if len(filtered) == 0 {
			println("no matches")
			continue
		}
		if len(filtered) == 1 {
			return filtered[0]
		}
		visible = filtered
	}
}

// trackDetails mirrors GetTracksMetadataJSON entries.
type trackDetails struct {
	URI        string `json:"uri"`
	Name       string `json:"name"`
	DurationMs int64  `json:"duration_ms"`
	ISRC       string `json:"isrc"`
	Album      *struct {
		Name     string `json:"name"`
		ImageURL string `json:"image_url"`
	} `json:"album"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Uid      string `json:"uid"`
	Provider string `json:"provider"`
}

func (t trackDetails) label() string {
	name := t.Name
	if name == "" {
		name = t.URI
	}
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	label := name
	if len(names) > 0 {
		label += " — " + strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s (%s)", label, fmtDuration(t.DurationMs))
}

func (s *shell) tracksMeta(uris []string) (map[string]trackDetails, error) {
	b, _ := json.Marshal(uris)
	raw, err := s.lib.GetTracksMetadataJSON(string(b))
	if err != nil {
		return nil, err
	}
	var list []trackDetails
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	out := make(map[string]trackDetails, len(list))
	for _, t := range list {
		out[t.URI] = t
	}
	return out, nil
}

type status struct {
	Playing    bool    `json:"playing"`
	Paused     bool    `json:"paused"`
	Stopped    bool    `json:"stopped"`
	Buffering  bool    `json:"buffering"`
	ContextUri string  `json:"context_uri"`
	TrackUri   string  `json:"track_uri"`
	PositionMs int64   `json:"position_ms"`
	DurationMs int64   `json:"duration_ms"`
	Shuffle    bool    `json:"shuffle"`
	RepeatMode string  `json:"repeat_mode"`
	Volume     int     `json:"volume"`
	VolumeMax  int     `json:"volume_max"`
	LocalGain  float64 `json:"local_gain"`
}

func (s *shell) status() (status, error) {
	var st status
	raw := s.lib.GetStatusJSON()
	if raw == "{}" {
		return st, fmt.Errorf("not started")
	}
	return st, json.Unmarshal([]byte(raw), &st)
}

// currentURI returns the playing track, for commands whose uri is optional.
func (s *shell) currentURI() (string, error) {
	st, err := s.status()
	if err != nil {
		return "", err
	}
	if st.TrackUri == "" {
		return "", fmt.Errorf("nothing is playing; give a uri")
	}
	return st.TrackUri, nil
}

func urisOrCurrent(s *shell, args []string) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}
	uri, err := s.currentURI()
	if err != nil {
		return nil, err
	}
	return []string{uri}, nil
}

func jsonList(items []string) string {
	b, _ := json.Marshal(items)
	return string(b)
}

func cmdOggDump(s *shell, a []string) error {
	if len(a) == 0 {
		state := "off"
		if s.lib.IsOggDumpEnabled() {
			state = "on"
		}
		printf("Ogg dump: %s (folder %q)\n", state, s.lib.GetOggDumpDir())
		return nil
	}
	on, ok := parseBool(a[0])
	if !ok || len(a) > 2 || (!on && len(a) > 1) {
		return errUsage
	}
	if !on {
		s.lib.DisableOggDump()
		println("Ogg dump: off")
		return nil
	}
	dir := ""
	if len(a) == 2 {
		dir = a[1]
	} else if s.lib.GetOggDumpDir() == "" {
		dir = "./ogg"
	}
	if err := s.lib.EnableOggDump(dir); err != nil {
		return err
	}
	printf("Ogg dump: on, saving to %s\n", s.lib.GetOggDumpDir())
	return nil
}

// OnOggWritten implements mobilebind.OggDumpCallback.
func (h *host) OnOggWritten(uri, path, infoJson string) {
	var info struct {
		Title   string   `json:"title"`
		Artists []string `json:"artists"`
		Size    int64    `json:"size"`
	}
	_ = json.Unmarshal([]byte(infoJson), &info)
	printf("[ogg] saved %s — %s (%d KiB) -> %s\n", strings.Join(info.Artists, ", "), info.Title, info.Size/1024, path)
}

// OnOggDumpError implements mobilebind.OggDumpCallback.
func (h *host) OnOggDumpError(uri, message string) {
	printf("[ogg] failed %s: %s\n", uri, message)
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "on", "true", "1", "yes":
		return true, true
	case "off", "false", "0", "no":
		return false, true
	}
	return false, false
}

func done(what string, err error) error {
	if err == nil {
		println(what)
	}
	return err
}

// splitLimit takes an optional trailing number off args.
func splitLimit(args []string, def int) ([]string, int) {
	if len(args) > 1 {
		if n, err := strconv.Atoi(args[len(args)-1]); err == nil {
			return args[:len(args)-1], n
		}
	}
	return args, def
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

var commands []*command

func init() {
	commands = []*command{
		// Playback
		{names: []string{"play"}, help: "Resume playback", run: func(s *shell, a []string) error {
			return done("Playing", s.lib.Play())
		}},
		{names: []string{"pause"}, help: "Pause playback", run: func(s *shell, a []string) error {
			return done("Paused", s.lib.Pause())
		}},
		{names: []string{"next", "n"}, help: "Next track", run: func(s *shell, a []string) error {
			return done("Next", s.lib.Next())
		}},
		{names: []string{"prev", "previous", "p"}, help: "Previous track (or restart if past ~3s)", run: func(s *shell, a []string) error {
			return done("Previous", s.lib.Previous())
		}},
		{names: []string{"seek"}, usage: "<ms|m:ss>", help: "Seek within the current track", run: cmdSeek},
		{names: []string{"status", "st"}, help: "Show what is playing and where", run: cmdStatus},
		{names: []string{"playuri"}, usage: "<uri> [skip_to_uri] [paused]", help: "Play a context (playlist, album, artist, track, ...)", run: cmdPlayURI},
		{names: []string{"playtracks"}, usage: "<start_index> <uri>...", help: "Play an explicit list of tracks as one queue", run: cmdPlayTracks},
		{names: []string{"jump"}, help: "Pick a track from the queue and skip to it", run: cmdJump},
		{names: []string{"repeat"}, usage: "off|context|track", help: "Set repeat mode", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			return done("Repeat: "+a[0], s.lib.SetRepeat(strings.ToLower(a[0])))
		}},
		{names: []string{"shuffle"}, usage: "on|off", help: "Shuffle the current context", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			on, ok := parseBool(a[0])
			if !ok {
				return errUsage
			}
			return done(fmt.Sprintf("Shuffle: %v", on), s.lib.SetShuffle(on))
		}},
		{names: []string{"volume", "vol"}, usage: "[n]", help: "Show or set the Connect volume (0..volume_steps)", run: cmdVolume},
		{names: []string{"oggdump"}, usage: "[on [dir] | off]", help: "Show or toggle saving each played track as a tagged .ogg", run: cmdOggDump},
		{names: []string{"gain"}, usage: "<0..1>", help: "Set the local-only gain (not seen by other devices)", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			g, err := strconv.ParseFloat(a[0], 32)
			if err != nil {
				return errUsage
			}
			s.lib.SetLocalGain(float32(g))
			printf("Local gain: %.2f\n", g)
			return nil
		}},
		{names: []string{"fade"}, usage: "<to 0..1> [ms]", help: "Ramp the local gain, e.g. to test ducking", run: cmdFade},

		// Queue
		{names: []string{"queue", "q"}, usage: "[json]", help: "Show the queue with metadata", run: cmdQueue},
		{names: []string{"addtoqueue", "aq"}, usage: "<uri>", help: "Append to the user queue", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			return done("Queued "+a[0], s.lib.AddToQueue(a[0]))
		}},
		{names: []string{"insertqueue", "iq"}, usage: "<index> <uri>", help: "Insert into the user queue (0 plays next)", run: func(s *shell, a []string) error {
			if len(a) != 2 {
				return errUsage
			}
			idx, err := strconv.Atoi(a[0])
			if err != nil {
				return errUsage
			}
			return done(fmt.Sprintf("Inserted %s at %d", a[1], idx), s.lib.InsertIntoQueue(a[1], idx))
		}},
		{names: []string{"removefromqueue", "rq"}, usage: "<uri>", help: "Remove the first matching upcoming track", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			return done("Removed "+a[0], s.lib.RemoveFromQueue(a[0]))
		}},
		{names: []string{"setqueue"}, usage: "<uri>...", help: "Replace everything after the current track", run: func(s *shell, a []string) error {
			if len(a) == 0 {
				return errUsage
			}
			return done(fmt.Sprintf("Upcoming set to %d tracks", len(a)), s.lib.SetQueueJSON(jsonList(a)))
		}},
		{names: []string{"movequeue", "mq"}, usage: "<from> <to>", help: "Move an upcoming track (positions as shown by queue)", run: cmdMoveQueue},
		{names: []string{"clearqueue"}, help: "Remove user-queued tracks", run: func(s *shell, a []string) error {
			s.lib.ClearQueue()
			println("Cleared user queue")
			return nil
		}},

		// Catalog
		{names: []string{"search", "s"}, usage: "<query> [limit]", help: "Search and play a result", run: cmdSearch},
		{names: []string{"suggest"}, usage: "<query> [limit]", help: "Type-ahead suggestions", run: func(s *shell, a []string) error {
			a, limit := splitLimit(a, 8)
			if len(a) == 0 {
				return errUsage
			}
			raw, err := s.lib.ApiSearchSuggestions(strings.Join(a, " "), limit)
			if err == nil {
				printJSON(raw)
			}
			return err
		}},
		{names: []string{"recent"}, usage: "[limit]", help: "Recent searches", run: func(s *shell, a []string) error {
			limit := 20
			if len(a) == 1 {
				n, err := strconv.Atoi(a[0])
				if err != nil {
					return errUsage
				}
				limit = n
			}
			raw, err := s.lib.ApiRecentSearches(limit)
			if err == nil {
				printJSON(raw)
			}
			return err
		}},
		{names: []string{"meta"}, usage: "[uri]...", help: "Batch track/episode metadata (default: current track)", run: func(s *shell, a []string) error {
			uris, err := urisOrCurrent(s, a)
			if err != nil {
				return err
			}
			raw, err := s.lib.GetTracksMetadataJSON(jsonList(uris))
			if err == nil {
				printJSON(raw)
			}
			return err
		}},
		{names: []string{"rawmeta"}, usage: "<uri>", help: "Raw metadata proto for any uri, as JSON", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			raw, err := s.lib.ApiGetMetadataForURI(a[0])
			if err == nil {
				printJSON(raw)
			}
			return err
		}},
		{names: []string{"album"}, usage: "<uri>", help: "Album details; pick a track to play", run: cmdAlbum},
		{names: []string{"artist"}, usage: "<uri>", help: "Artist details; pick a top track or release", run: cmdArtist},
		{names: []string{"lyrics"}, usage: "[uri]", help: "Lyrics (default: current track)", run: cmdLyrics},

		// Library
		{names: []string{"library", "lib"}, usage: "[json]", help: "Pick from your library and play", run: cmdLibrary},
		{names: []string{"liked"}, usage: "[json]", help: "Liked Songs; pick one to play the whole list from there", run: cmdLiked},
		{names: []string{"albums"}, usage: "[json]", help: "Saved albums; pick one to play", run: cmdSavedAlbums},
		{names: []string{"playlist", "pl"}, usage: "<uri> [known_revision] [json]", help: "Show a playlist (unchanged if the revision matches)", run: cmdPlaylist},
		{names: []string{"like"}, usage: "[uri]...", help: "Save to library (default: current track)", run: func(s *shell, a []string) error {
			uris, err := urisOrCurrent(s, a)
			if err != nil {
				return err
			}
			return done("Liked "+strings.Join(uris, " "), s.lib.Like(jsonList(uris)))
		}},
		{names: []string{"unlike"}, usage: "[uri]...", help: "Remove from library (default: current track)", run: func(s *shell, a []string) error {
			uris, err := urisOrCurrent(s, a)
			if err != nil {
				return err
			}
			return done("Unliked "+strings.Join(uris, " "), s.lib.Unlike(jsonList(uris)))
		}},
		{names: []string{"isliked"}, usage: "[uri]...", help: "Check library membership (default: current track)", run: func(s *shell, a []string) error {
			uris, err := urisOrCurrent(s, a)
			if err != nil {
				return err
			}
			raw, err := s.lib.IsLikedJSON(jsonList(uris))
			if err == nil {
				printJSON(raw)
			}
			return err
		}},
		{names: []string{"createplaylist"}, usage: "<name...>", help: "Create a playlist", run: func(s *shell, a []string) error {
			if len(a) == 0 {
				return errUsage
			}
			uri, err := s.lib.CreatePlaylist(strings.Join(a, " "))
			if uri != "" {
				println("Created", uri)
			}
			return err
		}},
		{names: []string{"renameplaylist"}, usage: "<uri> <name...>", help: "Rename a playlist", run: func(s *shell, a []string) error {
			if len(a) < 2 {
				return errUsage
			}
			rev, err := s.lib.RenamePlaylist(a[0], strings.Join(a[1:], " "))
			return done("Renamed; revision "+rev, err)
		}},
		{names: []string{"describeplaylist"}, usage: "<uri> <text...>", help: "Set a playlist's description", run: func(s *shell, a []string) error {
			if len(a) < 1 {
				return errUsage
			}
			rev, err := s.lib.SetPlaylistDescription(a[0], strings.Join(a[1:], " "))
			return done("Description set; revision "+rev, err)
		}},
		{names: []string{"addtoplaylist", "ap"}, usage: "<playlist> <index|end> [uri]...", help: "Add tracks (default: current track)", run: cmdAddToPlaylist},
		{names: []string{"removefromplaylist", "rp"}, usage: "<playlist> <index> [expected_uri]", help: "Remove the entry at index (0-based)", run: func(s *shell, a []string) error {
			if len(a) < 2 || len(a) > 3 {
				return errUsage
			}
			idx, err := strconv.Atoi(a[1])
			if err != nil {
				return errUsage
			}
			expected := ""
			if len(a) == 3 {
				expected = a[2]
			}
			rev, err := s.lib.RemoveFromPlaylist(a[0], idx, expected)
			return done(fmt.Sprintf("Removed entry %d; revision %s", idx, rev), err)
		}},
		{names: []string{"deleteplaylist"}, usage: "<uri>", help: "Remove a playlist from your library", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			if !s.confirm("Delete " + a[0] + "?") {
				println("Cancelled")
				return nil
			}
			return done("Deleted "+a[0], s.lib.DeletePlaylist(a[0]))
		}},

		// Devices
		{names: []string{"devices"}, help: "List Connect devices", run: func(s *shell, a []string) error {
			printJSON(s.lib.ListDevicesJSON())
			return nil
		}},
		{names: []string{"transfer"}, help: "Pick a device and move playback there", run: cmdTransfer},

		// Lifecycle & shell
		{names: []string{"bg"}, help: "Simulate the app going to the background", run: func(s *shell, a []string) error {
			return done("OnBackground", s.lib.OnBackground())
		}},
		{names: []string{"fg"}, help: "Simulate the app returning to the foreground", run: func(s *shell, a []string) error {
			return done("OnForeground", s.lib.OnForeground())
		}},
		{names: []string{"events"}, usage: "compact|full|off", help: "How bridge events are printed", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			mode := map[string]eventMode{"compact": eventsCompact, "full": eventsFull, "off": eventsOff}
			m, ok := mode[strings.ToLower(a[0])]
			if !ok {
				return errUsage
			}
			s.h.mu.Lock()
			s.h.events = m
			s.h.mu.Unlock()
			println("Events:", a[0])
			return nil
		}},
		{names: []string{"loglevel"}, usage: "debug|info|warn|error", help: "Minimum bridge log level shown", run: func(s *shell, a []string) error {
			if len(a) != 1 {
				return errUsage
			}
			if _, ok := logLevels[strings.ToLower(a[0])]; !ok {
				return errUsage
			}
			s.h.mu.Lock()
			s.h.logLevel = strings.ToLower(a[0])
			s.h.mu.Unlock()
			println("Log level:", a[0])
			return nil
		}},
		{names: []string{"help", "?"}, usage: "[command]", help: "Show commands", run: cmdHelp},
		{names: []string{"exit", "quit"}, help: "Stop librespot and exit (Ctrl+D also works)"},
	}
}

func findCommand(name string) *command {
	for _, c := range commands {
		for _, n := range c.names {
			if n == name {
				return c
			}
		}
	}
	return nil
}

func cmdHelp(s *shell, a []string) error {
	if len(a) == 1 {
		c := findCommand(strings.ToLower(a[0]))
		if c == nil {
			return fmt.Errorf("unknown command %q", a[0])
		}
		printf("%s %s\n  %s\n", strings.Join(c.names, ", "), c.usage, c.help)
		return nil
	}
	for _, c := range commands {
		head := c.names[0]
		if c.usage != "" {
			head += " " + c.usage
		}
		printf("  %-46s %s\n", head, c.help)
	}
	println("\nTab completes commands; ↑/↓ browse history. A uri can also be an open.spotify.com link.")
	return nil
}

func cmdSeek(s *shell, a []string) error {
	if len(a) != 1 {
		return errUsage
	}
	var ms int64
	if m, sec, ok := strings.Cut(a[0], ":"); ok {
		mi, err1 := strconv.ParseInt(m, 10, 64)
		si, err2 := strconv.ParseInt(sec, 10, 64)
		if err1 != nil || err2 != nil {
			return errUsage
		}
		ms = (mi*60 + si) * 1000
	} else {
		v, err := strconv.ParseInt(a[0], 10, 64)
		if err != nil {
			return errUsage
		}
		ms = v
	}
	return done("Seek to "+fmtPosition(ms), s.lib.Seek(ms))
}

func cmdStatus(s *shell, a []string) error {
	st, err := s.status()
	if err != nil {
		return err
	}
	state := "playing"
	switch {
	case st.Stopped:
		state = "stopped"
	case st.Buffering:
		state = "buffering"
	case st.Paused:
		state = "paused"
	}
	name := s.h.current()
	if name == "" {
		name = st.TrackUri
	}
	printf("%s  %s\n", state, name)
	if st.TrackUri != "" {
		printf("  %s / %s   %s\n", fmtPosition(st.PositionMs), fmtDuration(st.DurationMs), st.TrackUri)
	}
	if st.ContextUri != "" {
		printf("  context %s\n", st.ContextUri)
	}
	printf("  shuffle %v · repeat %s · volume %d/%d · local gain %.2f\n", st.Shuffle, st.RepeatMode, st.Volume, st.VolumeMax, st.LocalGain)
	return nil
}

func cmdPlayURI(s *shell, a []string) error {
	paused := false
	if len(a) > 0 && strings.EqualFold(a[len(a)-1], "paused") {
		paused, a = true, a[:len(a)-1]
	}
	if len(a) < 1 || len(a) > 2 {
		return errUsage
	}
	skip := ""
	if len(a) == 2 {
		skip = a[1]
	}
	return done("Playing "+a[0], s.lib.PlayUri(a[0], skip, paused))
}

func cmdPlayTracks(s *shell, a []string) error {
	if len(a) < 2 {
		return errUsage
	}
	start, err := strconv.Atoi(a[0])
	if err != nil {
		return errUsage
	}
	return done(fmt.Sprintf("Playing %d tracks from #%d", len(a)-1, start), s.lib.PlayTracks(jsonList(a[1:]), start, 0, false))
}

func cmdVolume(s *shell, a []string) error {
	if len(a) == 0 {
		printJSON(s.lib.GetVolumeJSON())
		return nil
	}
	n, err := strconv.ParseInt(a[0], 10, 32)
	if err != nil {
		return errUsage
	}
	if err := s.lib.SetVolume(int32(n)); err != nil {
		return err
	}
	var v struct{ Value, Max int }
	_ = json.Unmarshal([]byte(s.lib.GetVolumeJSON()), &v)
	if int(n) != v.Value {
		printf("Volume %d (clamped from %d; range 0..%d)\n", v.Value, n, v.Max)
	} else {
		printf("Volume %d\n", v.Value)
	}
	return nil
}

func cmdFade(s *shell, a []string) error {
	if len(a) < 1 || len(a) > 2 {
		return errUsage
	}
	to, err := strconv.ParseFloat(a[0], 64)
	if err != nil {
		return errUsage
	}
	dur := 1000
	if len(a) == 2 {
		if dur, err = strconv.Atoi(a[1]); err != nil {
			return errUsage
		}
	}
	st, err := s.status()
	if err != nil {
		return err
	}
	from := st.LocalGain
	const step = 20 * time.Millisecond
	steps := max(1, dur/int(step.Milliseconds()))
	for i := 1; i <= steps; i++ {
		s.lib.SetLocalGain(float32(from + (to-from)*float64(i)/float64(steps)))
		time.Sleep(step)
	}
	printf("Faded local gain %.2f → %.2f over %dms\n", from, to, dur)
	return nil
}

type queueJSON struct {
	ContextUri string         `json:"context_uri"`
	Current    *trackDetails  `json:"current"`
	Prev       []trackDetails `json:"prev"`
	Next       []trackDetails `json:"next"`
}

func (s *shell) queue() (*queueJSON, string, error) {
	raw := s.lib.GetQueueJSON()
	if raw == "{}" {
		return nil, raw, fmt.Errorf("not started")
	}
	var q queueJSON
	return &q, raw, json.Unmarshal([]byte(raw), &q)
}

func cmdQueue(s *shell, a []string) error {
	q, raw, err := s.queue()
	if err != nil {
		return err
	}
	if len(a) == 1 && a[0] == "json" {
		printJSON(raw)
		return nil
	}
	if q.ContextUri != "" {
		println("context", q.ContextUri)
	}
	for i, t := range q.Prev {
		printf("  -%-3d %s\n", len(q.Prev)-i, t.label())
	}
	if q.Current != nil {
		printf("  ▶    %s\n", q.Current.label())
	} else {
		println("  (nothing playing)")
	}
	for i, t := range q.Next {
		tag := ""
		if t.Provider == "queue" {
			tag = " [queued]"
		} else if t.Provider == "autoplay" {
			tag = " [autoplay]"
		}
		printf("  %-4d %s%s\n", i, t.label(), tag)
	}
	return nil
}

func cmdMoveQueue(s *shell, a []string) error {
	if len(a) != 2 {
		return errUsage
	}
	from, err1 := strconv.Atoi(a[0])
	to, err2 := strconv.Atoi(a[1])
	if err1 != nil || err2 != nil {
		return errUsage
	}
	q, _, err := s.queue()
	if err != nil {
		return err
	}
	uris := make([]string, len(q.Next))
	for i, t := range q.Next {
		uris[i] = t.URI
	}
	if from < 0 || from >= len(uris) || to < 0 || to >= len(uris) {
		return fmt.Errorf("positions must be between 0 and %d", len(uris)-1)
	}
	moved := uris[from]
	uris = append(uris[:from], uris[from+1:]...)
	uris = append(uris[:to], append([]string{moved}, uris[to:]...)...)
	return done(fmt.Sprintf("Moved %d → %d", from, to), s.lib.SetQueueJSON(jsonList(uris)))
}

func cmdJump(s *shell, a []string) error {
	q, _, err := s.queue()
	if err != nil {
		return err
	}
	var labels []string
	for i, t := range q.Prev {
		labels = append(labels, fmt.Sprintf("← %d  %s", len(q.Prev)-i, t.label()))
	}
	for i, t := range q.Next {
		labels = append(labels, fmt.Sprintf("→ %d  %s", i+1, t.label()))
	}
	idx := s.pick("Jump to:", labels)
	if idx < 0 {
		return nil
	}
	if idx < len(q.Prev) {
		return done("Jumped back", s.lib.SkipToPrevTrack(q.Prev[idx].URI))
	}
	return done("Jumped ahead", s.lib.SkipToNextTrack(q.Next[idx-len(q.Prev)].URI))
}

func cmdSearch(s *shell, a []string) error {
	a, limit := splitLimit(a, 10)
	if len(a) == 0 {
		return errUsage
	}
	raw, err := s.lib.ApiSearch(strings.Join(a, " "), limit)
	if err != nil {
		return err
	}
	var results []struct {
		SpotifyID   string `json:"spotify_id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Kind        string `json:"kind"`
		DurationMs  int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		return err
	}
	labels := make([]string, len(results))
	for i, r := range results {
		label := r.Title
		if r.Description != "" {
			label += " — " + truncate(r.Description, 60)
		}
		if r.Kind != "" {
			label = "[" + r.Kind + "] " + label
		}
		if r.DurationMs > 0 {
			label += " (" + fmtDuration(r.DurationMs) + ")"
		}
		labels[i] = label
	}
	idx := s.pick("Results:", labels)
	if idx < 0 {
		return nil
	}
	uri := results[idx].SpotifyID
	if !strings.HasPrefix(uri, "spotify:") {
		uri = "spotify:track:" + uri
	}
	return done("Playing "+uri, s.lib.PlayUri(uri, "", false))
}

func cmdAlbum(s *shell, a []string) error {
	if len(a) != 1 {
		return errUsage
	}
	raw, err := s.lib.GetAlbumJSON(a[0])
	if err != nil {
		return err
	}
	var album struct {
		URI         string         `json:"uri"`
		Name        string         `json:"name"`
		ReleaseDate string         `json:"release_date"`
		Label       string         `json:"label"`
		Tracks      []trackDetails `json:"tracks"`
	}
	if err := json.Unmarshal([]byte(raw), &album); err != nil {
		return err
	}
	printf("%s (%s) · %s\n", album.Name, album.ReleaseDate, album.Label)
	labels := make([]string, len(album.Tracks))
	for i, t := range album.Tracks {
		labels[i] = t.label()
	}
	idx := s.pick("Tracks:", labels)
	if idx < 0 {
		return nil
	}
	return done("Playing "+album.Tracks[idx].Name, s.lib.PlayUri(album.URI, album.Tracks[idx].URI, false))
}

func cmdArtist(s *shell, a []string) error {
	if len(a) != 1 {
		return errUsage
	}
	raw, err := s.lib.GetArtistJSON(a[0])
	if err != nil {
		return err
	}
	type release struct {
		URI  string `json:"uri"`
		Name string `json:"name"`
		Year int    `json:"year"`
	}
	var artist struct {
		Name      string         `json:"name"`
		TopTracks []trackDetails `json:"top_tracks"`
		Albums    []release      `json:"albums"`
		Singles   []release      `json:"singles"`
	}
	if err := json.Unmarshal([]byte(raw), &artist); err != nil {
		return err
	}
	println(artist.Name)
	var labels, uris []string
	for _, t := range artist.TopTracks {
		labels = append(labels, "♪ "+t.label())
		uris = append(uris, t.URI)
	}
	for _, r := range append(artist.Albums, artist.Singles...) {
		labels = append(labels, fmt.Sprintf("◉ %s (%d)", r.Name, r.Year))
		uris = append(uris, r.URI)
	}
	idx := s.pick("Top tracks and releases:", labels)
	if idx < 0 {
		return nil
	}
	return done("Playing "+uris[idx], s.lib.PlayUri(uris[idx], "", false))
}

func cmdLyrics(s *shell, a []string) error {
	uris, err := urisOrCurrent(s, a)
	if err != nil {
		return err
	}
	raw, err := s.lib.GetLyricsJSON(uris[0])
	if err != nil {
		return err
	}
	var ly struct {
		SyncType string `json:"sync_type"`
		Lines    []struct {
			StartMs int64  `json:"start_ms"`
			Words   string `json:"words"`
		} `json:"lines"`
	}
	if err := json.Unmarshal([]byte(raw), &ly); err != nil {
		return err
	}
	if len(ly.Lines) == 0 {
		println("No lyrics.")
		return nil
	}
	println("(" + ly.SyncType + ")")
	for _, l := range ly.Lines {
		if ly.SyncType == "UNSYNCED" {
			println("  " + l.Words)
		} else {
			printf("  [%s] %s\n", fmtDuration(l.StartMs), l.Words)
		}
	}
	return nil
}

func cmdLibrary(s *shell, a []string) error {
	raw, err := s.lib.ApiGetRootLibraryItems()
	if err != nil {
		return err
	}
	if len(a) == 1 && a[0] == "json" {
		printJSON(raw)
		return nil
	}
	var items []struct {
		URI         string `json:"uri"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Kind        string `json:"kind"`
		Length      int    `json:"length"`
		Folder      string `json:"folder"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return err
	}
	depth := map[string]int{} // folder uri -> nesting depth of its contents
	labels := make([]string, len(items))
	for i, it := range items {
		d := 0
		if it.Folder != "" {
			d = depth[it.Folder]
		}
		if it.Kind == "folder" {
			depth[it.URI] = d + 1
		}
		label := strings.Repeat("  ", d) + fmt.Sprintf("[%s] %s", it.Kind, it.Name)
		if it.Length > 0 {
			label += fmt.Sprintf(" · %d tracks", it.Length)
		}
		if it.Description != "" {
			label += " — " + truncate(it.Description, 50)
		}
		labels[i] = label
	}
	idx := s.pick("Library:", labels)
	if idx < 0 {
		return nil
	}
	if items[idx].Kind == "folder" {
		println("Folders cannot be played.")
		return nil
	}
	return done("Playing "+items[idx].Name, s.lib.PlayUri(items[idx].URI, "", false))
}

func cmdLiked(s *shell, a []string) error {
	raw, err := s.lib.GetCollectionTracksJSON()
	if err != nil {
		return err
	}
	if len(a) == 1 && a[0] == "json" {
		printJSON(raw)
		return nil
	}
	var items []struct {
		URI     string `json:"uri"`
		AddedAt int64  `json:"added_at"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return err
	}
	uris := make([]string, len(items))
	for i, it := range items {
		uris[i] = it.URI
	}
	meta, err := s.tracksMeta(uris)
	if err != nil {
		return err
	}
	printf("%d liked songs (metadata resolved for %d)\n", len(items), len(meta))
	labels := make([]string, len(items))
	for i, it := range items {
		t, ok := meta[it.URI]
		if !ok {
			t = trackDetails{URI: it.URI}
		}
		labels[i] = fmt.Sprintf("%s  %s", time.Unix(it.AddedAt, 0).Format("2006-01-02"), t.label())
	}
	idx := s.pick("Liked Songs (playing starts the whole list there):", labels)
	if idx < 0 {
		return nil
	}
	return done("Playing Liked Songs from #"+strconv.Itoa(idx+1), s.lib.PlayTracks(jsonList(uris), idx, 0, false))
}

func cmdSavedAlbums(s *shell, a []string) error {
	raw, err := s.lib.GetSavedAlbumsJSON()
	if err != nil {
		return err
	}
	if len(a) == 1 && a[0] == "json" {
		printJSON(raw)
		return nil
	}
	var albums []struct {
		URI     string `json:"uri"`
		Name    string `json:"name"`
		Year    int    `json:"year"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
	}
	if err := json.Unmarshal([]byte(raw), &albums); err != nil {
		return err
	}
	labels := make([]string, len(albums))
	for i, al := range albums {
		names := make([]string, 0, len(al.Artists))
		for _, ar := range al.Artists {
			names = append(names, ar.Name)
		}
		labels[i] = fmt.Sprintf("%s — %s (%d)", al.Name, strings.Join(names, ", "), al.Year)
	}
	idx := s.pick("Saved albums:", labels)
	if idx < 0 {
		return nil
	}
	return done("Playing "+albums[idx].Name, s.lib.PlayUri(albums[idx].URI, "", false))
}

func cmdPlaylist(s *shell, a []string) error {
	asJSON := len(a) > 0 && a[len(a)-1] == "json"
	if asJSON {
		a = a[:len(a)-1]
	}
	if len(a) < 1 || len(a) > 2 {
		return errUsage
	}
	known := ""
	if len(a) == 2 {
		known = a[1]
	}
	raw, err := s.lib.GetPlaylistJSON(a[0], known)
	if err != nil {
		return err
	}
	if asJSON {
		printJSON(raw)
		return nil
	}
	var pl struct {
		URI       string `json:"uri"`
		Name      string `json:"name"`
		Owner     string `json:"owner"`
		Revision  string `json:"revision"`
		Length    int    `json:"length"`
		Unchanged bool   `json:"unchanged"`
		Tracks    []struct {
			URI string `json:"uri"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal([]byte(raw), &pl); err != nil {
		return err
	}
	printf("%s by %s · %d entries · revision %s\n", pl.Name, pl.Owner, pl.Length, pl.Revision)
	if pl.Unchanged {
		println("Unchanged since the given revision.")
		return nil
	}
	uris := make([]string, len(pl.Tracks))
	for i, t := range pl.Tracks {
		uris[i] = t.URI
	}
	meta, err := s.tracksMeta(uris)
	if err != nil {
		return err
	}
	labels := make([]string, len(uris))
	for i, uri := range uris {
		t, ok := meta[uri]
		if !ok {
			t = trackDetails{URI: uri}
		}
		labels[i] = fmt.Sprintf("[%d] %s", i, t.label())
	}
	idx := s.pick("Tracks (index shown for removefromplaylist):", labels)
	if idx < 0 {
		return nil
	}
	return done("Playing "+pl.Name, s.lib.PlayUri(pl.URI, uris[idx], false))
}

func cmdAddToPlaylist(s *shell, a []string) error {
	if len(a) < 2 {
		return errUsage
	}
	pos := -1
	if a[1] != "end" {
		n, err := strconv.Atoi(a[1])
		if err != nil {
			return errUsage
		}
		pos = n
	}
	uris, err := urisOrCurrent(s, a[2:])
	if err != nil {
		return err
	}
	rev, err := s.lib.AddToPlaylist(a[0], jsonList(uris), pos)
	return done(fmt.Sprintf("Added %d; revision %s", len(uris), rev), err)
}

func cmdTransfer(s *shell, a []string) error {
	devices := s.lib.ApiListDevices()
	labels := make([]string, len(devices))
	for i, d := range devices {
		active := ""
		if d.IsActive {
			active = " (active)"
		}
		labels[i] = fmt.Sprintf("%s [%s]%s", d.Name, d.Type, active)
	}
	idx := s.pick("Transfer to:", labels)
	if idx < 0 {
		return nil
	}
	return done("Transfer requested to "+devices[idx].Name, s.lib.TransferPlayback(devices[idx].Id, true))
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

// normalizeArg turns open.spotify.com links into spotify: URIs.
func normalizeArg(arg string) string {
	const prefix = "https://open.spotify.com/"
	if !strings.HasPrefix(arg, prefix) {
		return arg
	}
	path := strings.TrimPrefix(arg, prefix)
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && strings.HasPrefix(parts[0], "intl-") {
		parts = parts[1:]
	}
	if len(parts) != 2 {
		return arg
	}
	return "spotify:" + parts[0] + ":" + parts[1]
}

func loadCredentials(path string) (username, data string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var payload struct {
		Username string `json:"username"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", fmt.Errorf("parsing %s: %w", path, err)
	}
	if payload.Username == "" || payload.Data == "" {
		return "", "", fmt.Errorf("%s is missing username or data", path)
	}
	return payload.Username, payload.Data, nil
}

func main() {
	credentialsPath := "./creds.json"

	cfg := map[string]any{
		"device_id":      "c3ad22f78186520b82da064a3da86f1566cbe559",
		"device_name":    "gomobile-host-sim",
		"device_type":    "smartphone",
		"audio_backend":  "oto",
		"volume_steps":   16,
		"initial_volume": 8,
		"credentials": map[string]any{
			"type":          "interactive",
			"spotify_token": map[string]any{"username": "", "data": ""},
		},
		"server":           map[string]any{"enabled": false},
		"zeroconf_enabled": false,
	}

	lib := mobilebind.NewLibrespot()
	h := &host{credentialsPath: credentialsPath, logLevel: "info"}
	lib.SetCredentialCallback(h)
	lib.SetOggDumpCallback(h)

	credentialsRestored := false
	if username, data, err := loadCredentials(credentialsPath); err == nil {
		cfg["credentials"].(map[string]any)["spotify_token"] = map[string]any{"username": username, "data": data}
		credentialsRestored = true
		fmt.Printf("Restoring saved credentials for %s\n", username)
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Println("Ignoring saved credentials:", err)
	}

	cfgBytes, _ := json.Marshal(cfg)
	if err := lib.StartWithConfigJSON(string(cfgBytes), h); err != nil {
		fmt.Fprintln(os.Stderr, "StartWithConfigJSON error:", err)
		os.Exit(1)
	}

	names := make([]string, 0, len(commands)*2)
	for _, c := range commands {
		names = append(names, c.names...)
	}
	sort.Strings(names)
	items := make([]readline.PrefixCompleterInterface, len(names))
	for i, n := range names {
		items[i] = readline.PcItem(n)
	}

	historyFile := ""
	if dir, err := os.UserCacheDir(); err == nil {
		historyFile = filepath.Join(dir, "go-librespot-mobile-host.history")
	}
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "librespot> ",
		HistoryFile:     historyFile,
		AutoComplete:    readline.NewPrefixCompleter(items...),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "readline:", err)
		os.Exit(1)
	}
	defer func() { _ = rl.Close() }()

	outMu.Lock()
	out = rl.Stdout()
	outMu.Unlock()

	s := &shell{lib: lib, h: h, rl: rl}

	if !credentialsRestored {
		authURL, err := lib.BeginInteractiveAuth()
		if err != nil {
			println("BeginInteractiveAuth error:", err)
		} else {
			println("Open this URL to log in, then paste the URL you are redirected to:")
			println(authURL)
			if cb := s.ask("redirect url (enter to skip)> "); cb != "" {
				if err := lib.CompleteInteractiveAuth(cb); err != nil {
					println("Login failed:", err)
				} else {
					println("Logged in. Restart mobile_host to start the player with the saved credentials.")
				}
			}
		}
	}

	println("Type help for commands, Tab to complete, Ctrl+D to exit.")
	for {
		line, err := rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			if line == "" {
				println("(Ctrl+D or exit to quit)")
			}
			continue
		} else if err != nil { // io.EOF
			break
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		verb := strings.ToLower(fields[0])
		args := fields[1:]
		for i := range args {
			args[i] = normalizeArg(args[i])
		}

		if verb == "exit" || verb == "quit" {
			break
		}
		c := findCommand(verb)
		if c == nil {
			printf("unknown command %q — type help\n", verb)
			continue
		}
		if err := c.run(s, args); errors.Is(err, errUsage) {
			printf("usage: %s %s\n", c.names[0], c.usage)
		} else if err != nil {
			printf("%s: %v\n", c.names[0], err)
		}
	}

	_ = lib.Stop()
	println("Bye.")
}
