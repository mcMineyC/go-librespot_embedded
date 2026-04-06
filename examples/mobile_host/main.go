package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	// Simple REPL: accept commands to control playback/lifecycle
	fmt.Println()
	fmt.Println("Enter commands: play, pause, next, prev, jump, bg, fg, stop, exit")
	for {
		prompt := promptui.Prompt{
			Label: "Command",
		}
		line, err := prompt.Run()
		if err != nil {
			fmt.Printf("Prompt failed: %v\n", err)
			continue
		}
		cmd := strings.TrimSpace(strings.ToLower(line))
		switch cmd {
		case "play":
			if err := lib.Play(); err != nil {
				fmt.Println("Play error:", err)
			} else {
				fmt.Println("Play requested")
			}
		case "pause":
			if err := lib.Pause(); err != nil {
				fmt.Println("Pause error:", err)
			} else {
				fmt.Println("Pause requested")
			}
		case "next":
			if err := lib.Next(); err != nil {
				fmt.Println("Next error:", err)
			} else {
				fmt.Println("Next requested")
			}
		case "prev", "previous":
			if err := lib.Previous(); err != nil {
				fmt.Println("Previous error:", err)
			} else {
				fmt.Println("Previous requested")
			}
		case "jump":
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
			fmt.Println(lib.ListDevicesJSON())
		case "transfer":
			devicesJSON := lib.ListDevicesJSON()
			var devices []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(devicesJSON), &devices); err != nil {
				fmt.Println("Failed parsing devices JSON:", err)
				break
			}
			if len(devices) == 0 {
				fmt.Println("No available devices to transfer to.")
				break
			}
			deviceLabels := make([]string, len(devices))
			for i, d := range devices {
				deviceLabels[i] = fmt.Sprintf("%s (%s)", d.Name, d.ID)
			}
			select_ := promptui.Select{
				Label: "Transfer playback to device",
				Items: deviceLabels,
				Size:  len(deviceLabels),
			}
			idx, _, err := select_.Run()
			if err != nil {
				fmt.Println("Transfer cancelled:", err)
				break
			}
			chosen := devices[idx]
			if err := lib.TransferPlayback(chosen.ID, true); err != nil {
				fmt.Println("Transfer error:", err)
			} else {
				fmt.Printf("Transfer requested to device: %s (%s)\n", chosen.Name, chosen.ID)
			}
		case "bg", "background":
			if err := lib.OnBackground(); err != nil {
				fmt.Println("OnBackground error:", err)
			} else {
				fmt.Println("OnBackground invoked")
			}
		case "fg", "foreground":
			if err := lib.OnForeground(); err != nil {
				fmt.Println("OnForeground error:", err)
			} else {
				fmt.Println("OnForeground invoked")
			}
		case "stop":
			if err := lib.Stop(); err != nil {
				fmt.Println("Stop error:", err)
			} else {
				fmt.Println("Stop requested")
			}
		case "exit", "quit":
			_ = lib.Stop()
			fmt.Println("Exiting.")
			return
		case "":
			// ignore empty
		default:
			fmt.Println("unknown command:", cmd)
		}
	}
}
