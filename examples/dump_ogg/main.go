// dump_ogg downloads a Spotify track and saves it as a tagged Ogg Vorbis file
// (metadata, Spotify ID, ReplayGain and cover art embedded).
//
// It reuses the credentials persisted by the mobile_host example:
//
//	go run ./examples/dump_ogg -creds examples/mobile_host/creds.json \
//	    https://open.spotify.com/track/3vej2q4zg54jED3p6vVjKz
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	daemon "github.com/devgianlu/go-librespot/daemoncore"
)

// toUri accepts spotify:track:… URIs and open.spotify.com links.
func toUri(s string) (string, error) {
	if strings.HasPrefix(s, "spotify:") {
		return s, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Links may carry a locale prefix, e.g. /intl-de/track/<id>.
	if len(parts) >= 2 && strings.HasPrefix(parts[0], "intl-") {
		parts = parts[1:]
	}
	if len(parts) != 2 || (parts[0] != "track" && parts[0] != "episode") {
		return "", fmt.Errorf("not a track or episode link: %s", s)
	}
	return "spotify:" + parts[0] + ":" + parts[1], nil
}

func run() error {
	credsPath := flag.String("creds", "examples/mobile_host/creds.json", "persisted credentials (written by mobile_host)")
	out := flag.String("o", "", "output file (default: <spotify id>.ogg)")
	bitrate := flag.Int("bitrate", 160, "preferred bitrate: 96, 160 or 320")
	flag.Parse()
	if flag.NArg() != 1 {
		return fmt.Errorf("usage: dump_ogg [-creds file] [-o out.ogg] <spotify url or uri>")
	}

	uri, err := toUri(flag.Arg(0))
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(*credsPath)
	if err != nil {
		return fmt.Errorf("reading credentials: %w", err)
	}
	var creds struct {
		Username string `json:"username"`
		Data     string `json:"data"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil || creds.Username == "" || creds.Data == "" {
		return fmt.Errorf("invalid credentials file %s (run mobile_host to log in first)", *credsPath)
	}

	cfgJson, _ := json.Marshal(map[string]any{
		"device_id":        "c3ad22f78186520b82da064a3da86f1566cbe559",
		"device_name":      "dump-ogg",
		"device_type":      "computer",
		"audio_backend":    "oto",
		"bitrate":          *bitrate,
		"volume_steps":     16,
		"initial_volume":   8,
		"zeroconf_enabled": false,
		"server":           map[string]any{"enabled": false},
		"credentials": map[string]any{
			"type":          "spotify_token",
			"spotify_token": map[string]any{"username": creds.Username, "data": creds.Data},
		},
	})
	cfg, err := daemon.ParseConfigJSON(string(cfgJson))
	if err != nil {
		return err
	}
	app, err := daemon.NewAppFromConfig(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p, err := app.NewAppPlayer(ctx)
	if err != nil {
		return err
	}
	if err := app.StartPlayer(ctx, p); err != nil {
		return err
	}

	st, err := p.NewEncodedStream(ctx, uri)
	if err != nil {
		return err
	}

	fmt.Printf("%s — %s (%s, %d bytes)\n", uri, st.Media.Name(), st.File.Format, st.Size)
	if st.ReplayGain != nil {
		fmt.Printf("replaygain: track %.2f dB (peak %.4f), album %.2f dB (peak %.4f)\n",
			st.ReplayGain.TrackGainDb, st.ReplayGain.TrackPeak, st.ReplayGain.AlbumGainDb, st.ReplayGain.AlbumPeak)
	}
	if n := st.TrackNormalization; n != nil {
		fmt.Printf("loudness: track %.2f LUFS (true peak %.2f dBTP)\n", n.LoudnessDb, n.TruePeakDb)
	}

	fname := *out
	if fname == "" {
		fname = st.Media.Id().Base62() + ".ogg"
	}
	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	if err := p.WriteTaggedOgg(ctx, st, f); err != nil {
		f.Close()
		_ = os.Remove(fname)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	fmt.Printf("saved %s\n", fname)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
