package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devgianlu/go-librespot/mobilebind"
	"github.com/manifoldco/promptui"
)

type cb struct {
	credentialsPath string
	currentName     string // updated on each metadata event
}

func (c *cb) OnEvent(event string, payloadJson string) {
	fmt.Printf("[STATE] %s -> %s\n", event, payloadJson)
	if event == "metadata" {
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(payloadJson), &meta); err == nil && meta.Name != "" {
			c.currentName = meta.Name
		}
	}
}

func (c *cb) OnLog(level string, message string) {
	fmt.Printf("[LOG] %s: %s\n", level, message)
}

// replHelpText lists every interactive command understood by the mobile_host REPL.
// Keep this in sync with the switch in main().
const replHelpText = `Available commands:
  play              Resume playback
  pause             Pause playback
  next              Skip to next track in context
  prev, previous    Previous track (or seek to start if past ~3s)
  jump              Pick a track from surrounding context (prev / current / next)
  seek <ms>         Seek to position in milliseconds from track start
  playuri <uri> [skip_to_uri] [paused]
                    Load a context URI; optional skip-to track URI; suffix "paused" to start paused
  addtoqueue <uri>  Append track or episode URI to the play-ahead queue
  removefromqueue <uri>
                    Remove first matching URI from the next queue
  getqueue, queue   Print current / prev / next queue (JSON, indented)
  clearqueue        Remove user-queued items (context sequence unchanged)
  getvolume         Print volume as JSON {"value","max"}
  setvolume <n>     Set volume step (0 .. volume_steps from config)
  setrepeat <mode>  off | context | track
  setshuffle on|off Shuffle for current context
  getmetadata <uri> Fetch metadata for a Spotify URI (track/album/artist/playlist)
  devices           Print Connect devices from cached cluster (JSON, indented)
  transfer          Choose a device and move playback here (starts playing)
  bg, background    Simulate app background (pause)
  fg, foreground    Simulate app foreground (resume)
  stop              Shut down librespot (cancels background work; session not usable until restart)
  exit, quit        Stop librespot and exit the program
  help, ?           Show this list

Notes:
  - devices / transfer need cluster data; play something from Spotify once if the list is empty.
  - For programmatic control, use mobilebind.Librespot (Api* methods mirror daemoncore).
`

func printReplHelp() {
	fmt.Print(replHelpText)
}

func (c *cb) OnCredentialsPersist(credentialsJson string) error {
	fmt.Println("OnCredsPersist!!!")
	if c.credentialsPath == "" {
		return fmt.Errorf("credentials path not set")
	}

	// Ensure the directory exists
	dir := filepath.Dir(c.credentialsPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed creating credentials directory: %w", err)
	}

	// Write credentials to file with restricted permissions
	if err := os.WriteFile(c.credentialsPath, []byte(credentialsJson), 0600); err != nil {
		return fmt.Errorf("failed writing credentials: %w", err)
	}

	fmt.Printf("Credentials persisted to: %s\n", c.credentialsPath)
	return nil
}

func main() {
	// Determine credentials storage path
	credentialsPath := "./creds.json"

	// Build a minimal mobile-friendly JSON config
	cfg := map[string]interface{}{
		"device_id":      "c3ad22f78186520b82da064a3da86f1566cbe559",
		"device_name":    "gomobile-host-sim",
		"device_type":    "smartphone",
		"audio_backend":  "oto",
		"volume_steps":   16,
		"initial_volume": 8,
		"credentials": map[string]interface{}{
			"type": "interactive",
			"spotify_token": map[string]interface{}{
				"username": "",
				"data":     "",
			},
		},
		"server": map[string]interface{}{
			"enabled": false,
		},
		"zeroconf_enabled": false,
	}

	// Create instance and start
	lib := mobilebind.NewLibrespot()
	callback := &cb{credentialsPath: credentialsPath}

	// Register credential callback before starting so the daemon will invoke the
	// host-provided persistence function instead of writing state.json.
	lib.SetCredentialCallback(callback)
	credentialsRestored := false
	if credsData, err := os.ReadFile(credentialsPath); err == nil {
		fmt.Println("Found persisted credentials, attempting to restore...")
		var payload struct {
			Username string `json:"username"`
			Data     string `json:"data"`
		}
		if err := json.Unmarshal([]byte(credsData), &payload); err != nil {
			fmt.Errorf("failed parsing credentials json: %w", err)
		} else if payload.Username == "" || payload.Data == "" {
			fmt.Errorf("Credentials json missing required fields, skipping restore")
		} else {
			cfg["credentials"].(map[string]interface{})["spotify_token"] = map[string]interface{}{
				"username": payload.Username,
				"data":     payload.Data,
			}
		}
		credentialsRestored = true
	}

	// Turn map into json
	cfgBytes, _ := json.MarshalIndent(cfg, "", "  ")
	configJSON := string(cfgBytes)

	fmt.Println("Using config:")
	fmt.Println(configJSON)
	fmt.Println()

	if err := lib.StartWithConfigJSON(configJSON, callback); err != nil {
		fmt.Fprintln(os.Stderr, "StartWithConfigJSON error:", err)
		os.Exit(1)
	}

	fmt.Println("librespot started (shim).")

	// Try to restore persisted credentials

	// Only proceed with OAuth flow if credentials were not restored
	if !credentialsRestored {
		// Begin interactive auth flow: host opens the returned URL in a browser/webview
		authURL, err := lib.BeginInteractiveAuth()
		if err != nil {
			fmt.Fprintln(os.Stderr, "BeginInteractiveAuth error:", err)
			// proceed - maybe already authenticated
		} else {
			fmt.Println("Open this URL in a browser/webview to authenticate:")
			fmt.Println(authURL)
		}

		// Wait for redirect callback (paste the redirect URL from browser)
		fmt.Println()
		fmt.Println("After authenticating in the browser, paste the redirect URL here (or press Enter to skip):")

		prompt := promptui.Prompt{
			Label:   "Callback URL",
			Default: "",
		}
		callbackURL, err := prompt.Run()
		if err != nil {
			fmt.Printf("Prompt failed: %v\n", err)
			callbackURL = ""
		}

		if callbackURL != "" {
			fmt.Println("Completing interactive auth...")
			if err := lib.CompleteInteractiveAuth(callbackURL); err != nil {
				fmt.Fprintln(os.Stderr, "CompleteInteractiveAuth error:", err)
			} else {
				fmt.Println("Interactive auth completed successfully!")
				// give core a moment to initialize session/player
				time.Sleep(500 * time.Millisecond)
			}
		} else {
			fmt.Println("No callback URL provided; continuing without interactive auth.")
		}
	}

	// Simple REPL: accept commands to control playback/lifecycle (see replHelpText).
	fmt.Println()
	fmt.Println("Interactive REPL — type help for commands, exit to quit.")
	for {
		prompt := promptui.Prompt{
			Label: "Command",
		}
		line, err := prompt.Run()
		if err != nil {
			fmt.Printf("Prompt failed: %v\n", err)
			continue
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		verb := strings.ToLower(fields[0])
		args := fields[1:]

		switch verb {
		case "play":
			if len(args) != 0 {
				fmt.Println("usage: play")
				break
			}
			if err := lib.Play(); err != nil {
				fmt.Println("Play error:", err)
			} else {
				fmt.Println("Play requested")
			}
		case "pause":
			if len(args) != 0 {
				fmt.Println("usage: pause")
				break
			}
			if err := lib.Pause(); err != nil {
				fmt.Println("Pause error:", err)
			} else {
				fmt.Println("Pause requested")
			}
		case "next":
			if len(args) != 0 {
				fmt.Println("usage: next")
				break
			}
			if err := lib.Next(); err != nil {
				fmt.Println("Next error:", err)
			} else {
				fmt.Println("Next requested")
			}
		case "prev", "previous":
			if len(args) != 0 {
				fmt.Println("usage: prev")
				break
			}
			if err := lib.Previous(); err != nil {
				fmt.Println("Previous error:", err)
			} else {
				fmt.Println("Previous requested")
			}
		case "seek":
			if len(args) < 1 {
				fmt.Println("usage: seek <ms>")
				break
			}
			ms, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				fmt.Println("seek: invalid milliseconds:", err)
				break
			}
			if err := lib.ApiSeek(ms); err != nil {
				fmt.Println("Seek error:", err)
			} else {
				fmt.Printf("Seek to %d ms\n", ms)
			}
		case "playuri":
			if len(args) < 1 {
				fmt.Println("usage: playuri <context_uri> [skip_to_uri] [paused]")
				break
			}
			paused := false
			rest := args
			if len(rest) > 0 && strings.EqualFold(rest[len(rest)-1], "paused") {
				paused = true
				rest = rest[:len(rest)-1]
			}
			uri := rest[0]
			skip := ""
			if len(rest) >= 2 {
				skip = rest[1]
			}
			if err := lib.ApiPlayUri(uri, skip, paused); err != nil {
				fmt.Println("playuri error:", err)
			} else {
				fmt.Println("playuri requested:", uri)
			}
		case "addtoqueue":
			if len(args) < 1 {
				fmt.Println("usage: addtoqueue <uri>")
				break
			}
			uri := args[0]
			if err := lib.ApiAddToQueue(uri); err != nil {
				fmt.Println("addtoqueue error:", err)
			} else {
				fmt.Println("Added to queue:", uri)
			}
		case "removefromqueue":
			if len(args) < 1 {
				fmt.Println("usage: removefromqueue <uri>")
				break
			}
			uri := args[0]
			if err := lib.ApiRemoveFromQueue(uri); err != nil {
				fmt.Println("removefromqueue error:", err)
			} else {
				fmt.Println("Removed from queue:", uri)
			}
		case "getqueue", "queue":
			if len(args) != 0 {
				fmt.Println("usage: getqueue")
				break
			}
			raw := lib.GetQueueJSON()
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				fmt.Println(raw)
				break
			}
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				fmt.Println(raw)
				break
			}
			fmt.Println(string(b))
		case "clearqueue":
			if len(args) != 0 {
				fmt.Println("usage: clearqueue")
				break
			}
			lib.ApiClearQueue()
			fmt.Println("Cleared user queue (context tracks kept).")
		case "getvolume":
			if len(args) != 0 {
				fmt.Println("usage: getvolume")
				break
			}
			raw := lib.GetVolumeJSON()
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				fmt.Println(raw)
				break
			}
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				fmt.Println(raw)
				break
			}
			fmt.Println(string(b))
		case "setvolume":
			if len(args) < 1 {
				fmt.Println("usage: setvolume <n>")
				break
			}
			n, err := strconv.ParseInt(args[0], 10, 32)
			if err != nil {
				fmt.Println("setvolume: invalid number:", err)
				break
			}
			if err := lib.ApiSetVolume(int32(n)); err != nil {
				fmt.Println("setvolume error:", err)
			} else {
				fmt.Printf("Volume set to %d\n", n)
			}
		case "setrepeat":
			if len(args) < 1 {
				fmt.Println("usage: setrepeat off|context|track")
				break
			}
			mode := strings.ToLower(args[0])
			if err := lib.ApiSetRepeat(mode); err != nil {
				fmt.Println("setrepeat error:", err)
			} else {
				fmt.Println("Repeat mode:", mode)
			}
		case "setshuffle":
			if len(args) < 1 {
				fmt.Println("usage: setshuffle on|off")
				break
			}
			var on bool
			ok := false
			switch strings.ToLower(args[0]) {
			case "on", "true", "1", "yes":
				on, ok = true, true
			case "off", "false", "0", "no":
				on, ok = false, true
			}
			if !ok {
				fmt.Println("setshuffle: use on or off")
				break
			}
			if err := lib.ApiSetShuffle(on); err != nil {
				fmt.Println("setshuffle error:", err)
			} else {
				fmt.Printf("Shuffle: %v\n", on)
			}
		case "getmetadata":
			if len(args) < 1 {
				fmt.Println("usage: getmetadata <uri>")
				break
			}
			raw, err := lib.ApiGetMetadataForURI(args[0])
			if err != nil {
				fmt.Println("getmetadata error:", err)
				break
			}
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				fmt.Println(raw)
				break
			}
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				fmt.Println(raw)
				break
			}
			fmt.Println(string(b))
		case "jump":
			if len(args) != 0 {
				fmt.Println("usage: jump")
				break
			}
			prev, next := lib.ContextTracks(5)
			if len(prev) == 0 && len(next) == 0 {
				fmt.Println("No surrounding tracks available (is something playing?)")
				break
			}

			// Build a flat list with labels, tracking which index is the
			// boundary between prev and next so we can annotate it.
			type entry struct {
				label string
				uri   string
			}
			// Resolve the current track name from the most recent metadata event.
			currentLabel := "(current track)"
			if callback.currentName != "" {
				currentLabel = "▶  " + callback.currentName
			}
			const currentURI = "" // sentinel: selecting this is a no-op

			var items []entry
			for i, t := range prev {
				label := t.URI
				if t.Name != "" {
					label = t.Name
				}
				items = append(items, entry{fmt.Sprintf("← %d  %s", len(prev)-i, label), t.URI})
			}
			items = append(items, entry{currentLabel, currentURI})
			prevBoundary := len(items) // items[prevBoundary] will be the first next-track
			for i, t := range next {
				label := t.URI
				if t.Name != "" {
					label = t.Name
				}
				items = append(items, entry{fmt.Sprintf("→ %d  %s", i+1, label), t.URI})
			}

			labels := make([]string, len(items))
			for i, it := range items {
				labels[i] = it.label
			}

			select_ := promptui.Select{
				Label: "Jump to track",
				Items: labels,
				Size:  len(labels),
			}
			idx, _, err := select_.Run()
			if err != nil {
				fmt.Println("Jump cancelled:", err)
				break
			}
			chosen := items[idx]
			if chosen.uri == currentURI {
				fmt.Println("Already on this track.")
				break
			}
			var jumpErr error
			if idx < len(prev) {
				jumpErr = lib.SkipToPrevTrack(chosen.uri)
			} else if idx >= prevBoundary {
				jumpErr = lib.SkipToNextTrack(chosen.uri)
			}
			if jumpErr != nil {
				fmt.Println("Jump error:", jumpErr)
			} else {
				fmt.Println("Jumped to:", chosen.label)
			}
		case "devices":
			if len(args) != 0 {
				fmt.Println("usage: devices")
				break
			}
			devs := lib.ApiListDevices()
			if len(devs) == 0 {
				fmt.Println("No devices yet (cluster empty). Start playback from Spotify once to populate.")
				break
			}
			type row struct {
				ID             string `json:"id"`
				Name           string `json:"name"`
				Type           string `json:"type"`
				IsActive       bool   `json:"is_active"`
				Volume         int    `json:"volume"`
				SupportsVolume bool   `json:"supports_volume"`
			}
			rows := make([]row, len(devs))
			for i, d := range devs {
				rows[i] = row{
					ID:             d.Id,
					Name:           d.Name,
					Type:           d.Type,
					IsActive:       d.IsActive,
					Volume:         d.Volume,
					SupportsVolume: d.SupportsVolume,
				}
			}
			b, err := json.MarshalIndent(rows, "", "  ")
			if err != nil {
				fmt.Println("devices:", err)
				break
			}
			fmt.Println(string(b))
		case "transfer":
			if len(args) != 0 {
				fmt.Println("usage: transfer")
				break
			}
			devices := lib.ApiListDevices()
			if len(devices) == 0 {
				fmt.Println("No devices to transfer to.")
				break
			}
			labels := make([]string, len(devices))
			for i, d := range devices {
				active := ""
				if d.IsActive {
					active = " *active*"
				}
				labels[i] = fmt.Sprintf("%s (%s)%s", d.Name, d.Id, active)
			}
			select_ := promptui.Select{
				Label: "Transfer playback to device",
				Items: labels,
				Size:  len(labels),
			}
			idx, _, err := select_.Run()
			if err != nil {
				fmt.Println("Transfer cancelled:", err)
				break
			}
			chosen := devices[idx]
			if err := lib.ApiTransferPlayback(chosen.Id, true); err != nil {
				fmt.Println("Transfer error:", err)
			} else {
				fmt.Printf("Transfer requested to: %s (%s)\n", chosen.Name, chosen.Id)
			}
		case "bg", "background":
			if len(args) != 0 {
				fmt.Println("usage: bg")
				break
			}
			if err := lib.OnBackground(); err != nil {
				fmt.Println("OnBackground error:", err)
			} else {
				fmt.Println("OnBackground invoked")
			}
		case "fg", "foreground":
			if len(args) != 0 {
				fmt.Println("usage: fg")
				break
			}
			if err := lib.OnForeground(); err != nil {
				fmt.Println("OnForeground error:", err)
			} else {
				fmt.Println("OnForeground invoked")
			}
		case "stop":
			if len(args) != 0 {
				fmt.Println("usage: stop")
				break
			}
			if err := lib.Stop(); err != nil {
				fmt.Println("Stop error:", err)
			} else {
				fmt.Println("Stop requested")
			}
		case "exit", "quit":
			if len(args) != 0 {
				fmt.Println("usage: exit")
				break
			}
			_ = lib.Stop()
			fmt.Println("Exiting.")
			return
		case "help", "?":
			if len(args) != 0 {
				fmt.Println("usage: help")
				break
			}
			printReplHelp()
		default:
			fmt.Println("unknown command:", verb, "— type help for a list")
		}
	}
}
