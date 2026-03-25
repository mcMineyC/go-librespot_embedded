package daemoncore

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/mpris"
	"github.com/devgianlu/go-librespot/playplay"

	"github.com/devgianlu/go-librespot/apresolve"
	"github.com/devgianlu/go-librespot/player"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
	"github.com/devgianlu/go-librespot/session"
	"github.com/devgianlu/go-librespot/zeroconf"
	"github.com/gofrs/flock"
	log "github.com/sirupsen/logrus"
	"golang.org/x/exp/rand"

	"github.com/knadh/koanf/v2"
	flag "github.com/spf13/pflag"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
)

var errAlreadyRunning = errors.New("go-librespot is already running")

// CredentialPersistCallback is invoked when credentials are successfully obtained and should be persisted.
// The callback receives the username and serialized credentials data as a JSON string.
//
// When a callback is registered via SetCredentialPersistCallback, the daemon will:
// - Invoke this callback with credentials whenever they are obtained (e.g., after OAuth, zeroconf auth)
// - NOT write credentials to state.json (credential persistence is entirely the callback's responsibility)
//
// If no callback is registered, the daemon will persist credentials to state.json in the config directory.
//
// The credentialsJson parameter is a JSON-encoded object with fields:
//
//	{"username": "...", "data": [...]}
//
// where "data" is the opaque stored credential data from the session.
//
// The callback should return nil on success or an error on failure. Returning an error does NOT abort
// the authentication flow; it is logged as a warning.
type CredentialPersistCallback func(username string, credentialsJson string) error

type App struct {
	log librespot.Logger
	cfg *Config

	client *http.Client

	resolver *apresolve.ApResolver

	deviceId    string
	deviceType  devicespb.DeviceType
	clientToken string
	state       librespot.AppState

	server   ApiServer
	mpris    mpris.Server
	logoutCh chan *AppPlayer

	credentialPersistCb CredentialPersistCallback
}

func parseDeviceType(val string) (devicespb.DeviceType, error) {
	valEnum, ok := devicespb.DeviceType_value[strings.ToUpper(val)]
	if !ok {
		return 0, fmt.Errorf("invalid device type: %s", val)
	}

	return devicespb.DeviceType(valEnum), nil
}

func NewApp(cfg *Config) (app *App, err error) {
	app = &App{cfg: cfg, logoutCh: make(chan *AppPlayer)}

	logger := log.StandardLogger()
	logger.SetFormatter(&log.TextFormatter{
		DisableTimestamp: cfg.LogDisableTimestamp,
	})

	app.log = &LogrusAdapter{log.NewEntry(logger)}
	app.client = &http.Client{Timeout: 30 * time.Second}

	app.deviceType, err = parseDeviceType(cfg.DeviceType)
	if err != nil {
		return nil, err
	}

	app.state.SetLogger(app.log)
	app.state.Credentials.Username = cfg.Credentials.SpotifyToken.Username
	if cfg.Credentials.SpotifyToken.Data != "" {
		decoded, err := base64.StdEncoding.DecodeString(cfg.Credentials.SpotifyToken.Data)
		if err != nil {
			// fallback: treat as raw bytes (backwards compat)
			app.state.Credentials.Data = []byte(cfg.Credentials.SpotifyToken.Data)
		} else {
			app.state.Credentials.Data = decoded
		}
	}

	/* Deprecated in faor of passing in persisted state from the host:
	if err := app.state.Read(cfg.ConfigDir); err != nil {
		return nil, err
	}*/

	app.resolver = apresolve.NewApResolver(app.log, app.client)

	if cfg.DeviceId != "" {
		// Use configured device ID.
		app.deviceId = cfg.DeviceId
	} else if app.state.DeviceId != "" {
		// Use device ID generated in a previous run.
		app.deviceId = app.state.DeviceId
	} else {
		// Generate a new device ID.
		deviceIdBytes := make([]byte, 20)
		_, _ = rand.Read(deviceIdBytes)
		app.deviceId = hex.EncodeToString(deviceIdBytes)
		log.Infof("generated new device id: %s", app.deviceId)

		// Save device ID so we can reuse it next time.
		app.state.DeviceId = app.deviceId

		if err := app.state.Write(); err != nil {
			return nil, err
		}
	}

	if cfg.ClientToken != "" {
		app.clientToken = cfg.ClientToken
	}

	if cfg.FlacEnabled && !playplay.Plugin.IsSupported() {
		// FLAC decryption keys are available only with the PlayPlay DRM implementation.
		// Using PlayPlay might get you banned by Spotify.
		return nil, fmt.Errorf("FLAC playback requires a PlayPlay implementation")
	}

	return app, nil
}

// SetCredentialPersistCallback sets a callback that will be invoked when credentials
// are successfully obtained and should be persisted by the host.
//
// When a callback is set:
// - The daemon will invoke this callback with serialized credentials whenever they are obtained
// - The daemon will NOT write state.json; credential persistence becomes the callback's responsibility
// - The callback should implement custom storage (e.g., write to a file, secure keystore, etc.)
//
// When no callback is set:
// - The daemon will persist credentials to state.json in the config directory
// - This is the default behavior for backwards compatibility
//
// The callback can be set either before or after the app is started.
func (app *App) SetCredentialPersistCallback(cb CredentialPersistCallback) {
	app.credentialPersistCb = cb
}

// LoadStateJSON loads persisted state provided by the embedding application.
// The host application (embedding this library) is responsible for reading
// persisted bytes from disk or secure storage and passing them here.
//
// This method forwards the provided JSON bytes to the internal AppState loader
// and will populate in-memory state accordingly. The daemon will not attempt
// any file I/O for state when using this approach.
func (app *App) LoadStateJSON(data []byte) error {
	if app == nil {
		return fmt.Errorf("app is nil")
	}
	// Ensure logger is set on the state so LoadFromJSON can emit logs if needed.
	app.state.SetLogger(app.log)
	if err := app.state.LoadFromJSON(data); err != nil {
		return fmt.Errorf("failed loading state json: %w", err)
	}
	return nil
}

// DumpStateJSON returns the current in-memory state as JSON bytes. The embedding
// application should call this and persist the returned bytes (for example to a
// file or secure keystore). The daemon will not persist state to disk when a
// host-managed persistence model is used.
func (app *App) DumpStateJSON() ([]byte, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}
	return app.state.DumpToJSON()
}

// RestoreStoredCredentials restores stored session credentials previously serialized
// by the host or saved in state.json. The function accepts a JSON string which may
// be in one of the following forms:
//
//  1. Access token form (produced by some hosts):
//     {"username":"...","access_token":"..."}
//
//  2. Stored credentials form (the daemon's internal form):
//     {"username":"...","data":"<base64-or-bytes-json>"}  // "data" is JSON-marshalled []byte
//
// The method tries to handle both formats and will initialize a session using the
// provided credentials. It delegates to existing helper paths (SpotifyToken or
// withCredentials with session.StoredCredentials) so behavior is consistent with
// normal interactive auth flows.

func (app *App) newAppPlayer(ctx context.Context, creds any) (_ *AppPlayer, err error) {
	appPlayer := &AppPlayer{
		app:          app,
		stop:         make(chan struct{}, 1),
		logout:       app.logoutCh,
		countryCode:  new(string),
		volumeUpdate: make(chan float32, 1),
	}

	appPlayer.prefetchTimer = time.NewTimer(math.MaxInt64)
	appPlayer.prefetchTimer.Stop()

	fmt.Printf("Creds type: %T", creds)
	if appPlayer.sess, err = session.NewSessionFromOptions(ctx, &session.Options{
		Log:         app.log,
		DeviceType:  app.deviceType,
		DeviceId:    app.deviceId,
		ClientToken: app.clientToken,
		Resolver:    app.resolver,
		Client:      app.client,
		AppState:    &app.state,
		Credentials: creds,
	}); err != nil {
		return nil, err
	}

	appPlayer.initState()

	if appPlayer.player, err = player.NewPlayer(&player.Options{
		Spclient: appPlayer.sess.Spclient(),
		AudioKey: appPlayer.sess.AudioKey(),
		Events:   appPlayer.sess.Events(),
		Log:      app.log,

		FlacEnabled: app.cfg.FlacEnabled,

		NormalisationEnabled:      !app.cfg.NormalisationDisabled,
		NormalisationUseAlbumGain: app.cfg.NormalisationUseAlbumGain,
		NormalisationPregain:      app.cfg.NormalisationPregain,

		CountryCode: appPlayer.countryCode,

		AudioBackend:     app.cfg.AudioBackend,
		AudioDevice:      app.cfg.AudioDevice,
		MixerDevice:      app.cfg.MixerDevice,
		MixerControlName: app.cfg.MixerControlName,

		AudioBufferTime:  app.cfg.AudioBufferTime,
		AudioPeriodCount: app.cfg.AudioPeriodCount,

		ExternalVolume: app.cfg.ExternalVolume,
		VolumeUpdate:   appPlayer.volumeUpdate,

		AudioOutputPipe:       app.cfg.AudioOutputPipe,
		AudioOutputPipeFormat: app.cfg.AudioOutputPipeFormat,
	},
	); err != nil {
		return nil, fmt.Errorf("failed initializing player: %w", err)
	}

	return appPlayer, nil
}

// NewAppPlayer is an exported helper that constructs and returns an AppPlayer
// using persisted credentials if present. This provides a stable exported API
// for consumers (like the mobile bridge) to obtain a player instance. If no
// persisted credentials exist, an explicit error is returned to indicate an
// interactive auth flow is required.
func (app *App) NewAppPlayer(ctx context.Context) (*AppPlayer, error) {
	// Prefer persisted zeroconf credentials if present
	if app != nil && app.cfg != nil {
		if app.state.Credentials.Data != nil && len(app.state.Credentials.Data) > 0 {
			creds := session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			}
			return app.newAppPlayer(ctx, creds)
		}
	}

	// No persisted credentials available: instruct callers to perform interactive auth.
	return nil, fmt.Errorf("no stored credentials available: perform interactive auth or supply credentials")
}

// StartPlayer starts the provided AppPlayer's runtime loops. It wires the App's
// ApiServer and MPRIS receive channels into the player's Run method and starts
// it in a new goroutine. This is exported to allow external callers (e.g.
// mobile bridge) to start player processing once they have an AppPlayer.
func (app *App) StartPlayer(ctx context.Context, p *AppPlayer) error {
	if p == nil {
		return fmt.Errorf("player is nil")
	}

	// Resolve API receive channel (use a closed channel as fallback to avoid blocking)
	var apiRecv <-chan ApiRequest
	if app.server != nil {
		apiRecv = app.server.Receive()
	} else {
		apiRecv = make(<-chan ApiRequest)
	}

	// Resolve MPRIS receive channel (use a closed channel as fallback)
	var mprisRecv <-chan mpris.MediaPlayer2PlayerCommand
	if app.mpris != nil {
		mprisRecv = app.mpris.Receive()
	} else {
		mprisRecv = make(<-chan mpris.MediaPlayer2PlayerCommand)
	}

	// Start player run loop in background.
	go p.Run(ctx, apiRecv, mprisRecv)

	return nil
}

func (app *App) Zeroconf(ctx context.Context) error {
	return app.withAppPlayer(ctx, func(ctx context.Context) (*AppPlayer, error) {
		if app.cfg.Credentials.Zeroconf.PersistCredentials && len(app.state.Credentials.Data) > 0 {
			app.log.WithField("username", librespot.ObfuscateUsername(app.state.Credentials.Username)).
				Infof("loading previously persisted zeroconf credentials")
			return app.newAppPlayer(ctx, session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			})
		}

		return nil, nil
	})
}

func (app *App) SpotifyToken(ctx context.Context, username, token string) error {
	return app.withCredentials(ctx, session.SpotifyTokenCredentials{Username: username, Token: token})
}

func (app *App) Interactive(ctx context.Context, callbackPort int) error {
	return app.withCredentials(ctx, session.InteractiveCredentials{CallbackPort: callbackPort})
}

func (app *App) withCredentials(ctx context.Context, creds any) (err error) {
	return app.withAppPlayer(ctx, func(ctx context.Context) (*AppPlayer, error) {
		if len(app.state.Credentials.Data) > 0 {
			fmt.Printf("Using existing stored credentials for user %s\n", librespot.ObfuscateUsername(app.state.Credentials.Username))
			return app.newAppPlayer(ctx, session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			})
		} else {
			appPlayer, err := app.newAppPlayer(ctx, creds)
			if err != nil {
				return nil, err
			}

			// store credentials outside this context in case we get called again
			app.state.Credentials.Username = appPlayer.sess.Username()
			app.state.Credentials.Data = appPlayer.sess.StoredCredentials()

			// Invoke credential persist callback if provided.
			// If callback is set, the host is responsible for persisting credentials;
			// we do not write state.json in this case.
			if app.credentialPersistCb != nil {
				credsJson, _ := json.Marshal(map[string]interface{}{
					"username": app.state.Credentials.Username,
					"data":     app.state.Credentials.Data,
				})
				if err := app.credentialPersistCb(app.state.Credentials.Username, string(credsJson)); err != nil {
					app.log.WithError(err).Warn("credential persist callback failed")
					// Don't fail the auth flow if the callback fails; host can retry persistence later
				}
			} else {
				// No callback provided: persist to state.json (backwards compatibility)
				if err = app.state.Write(); err != nil {
					return nil, err
				}
			}

			app.log.WithField("username", librespot.ObfuscateUsername(appPlayer.sess.Username())).
				Debugf("stored credentials")
			return appPlayer, nil
		}
	})
}

func (app *App) withAppPlayer(ctx context.Context, appPlayerFunc func(context.Context) (*AppPlayer, error)) (err error) {
	// if zeroconf is disabled, there is not much we need to do
	if !app.cfg.ZeroconfEnabled {
		appPlayer, err := appPlayerFunc(ctx)
		if err != nil {
			return err
		} else if appPlayer == nil {
			panic("zeroconf is disabled and no credentials are present")
		}

		appPlayer.Run(ctx, app.server.Receive(), app.mpris.Receive())
		return nil
	}

	// pre fetch resolver endpoints
	if err := app.resolver.FetchAll(ctx); err != nil {
		return fmt.Errorf("failed getting endpoints from resolver: %w", err)
	}

	// start zeroconf server and dispatch
	z, err := zeroconf.NewZeroconf(app.log, app.cfg.ZeroconfPort, app.cfg.DeviceName, app.deviceId, app.deviceType, app.cfg.ZeroconfInterfacesToAdvertise, app.cfg.ZeroconfBackend == "avahi")
	if err != nil {
		return fmt.Errorf("failed initializing zeroconf: %w", err)
	}

	var apiCh chan ApiRequest

	currentPlayer, err := appPlayerFunc(ctx)
	if err != nil {
		return err
	}

	// set current player to the provided one if we have it
	if currentPlayer != nil {
		app.log.WithField("username", librespot.ObfuscateUsername(currentPlayer.sess.Username())).
			Debugf("initializing zeroconf session")

		apiCh = make(chan ApiRequest)
		go currentPlayer.Run(ctx, apiCh, app.mpris.Receive())

		// let zeroconf know that we already have a user
		z.SetCurrentUser(currentPlayer.sess.Username())
	}

	// forward API requests to proper channel only if a session is present
	go func() {
		for {
			select {
			case req := <-app.server.Receive():
				if currentPlayer == nil {
					req.Reply(nil, ErrNoSession)
					break
				}

				// if we are here the channel must exist
				apiCh <- req
			}
		}
	}()

	// listen for logout events and unset session when that happens
	go func() {
		for {
			select {
			case p := <-app.logoutCh:
				// check that the logout request is for the current player
				if p != currentPlayer {
					continue
				}

				currentPlayer.Close()
				currentPlayer = nil

				// close the channel after setting the current session to nil
				close(apiCh)

				// restore the session if there is one.
				// we will restore the session even if it's for the same user, but it shouldn't be an issue
				newAppPlayer, err := appPlayerFunc(ctx)
				if err != nil {
					log.WithError(err).Errorf("failed restoring session after logout")

					// unset the zeroconf user
					z.SetCurrentUser("")
				} else if newAppPlayer == nil {
					// unset the zeroconf user
					z.SetCurrentUser("")
				} else {
					// first create the channel and then assign the current session
					apiCh = make(chan ApiRequest)
					currentPlayer = newAppPlayer

					go newAppPlayer.Run(ctx, apiCh, app.mpris.Receive())

					// let zeroconf know that we already have a user
					z.SetCurrentUser(newAppPlayer.sess.Username())

					app.log.WithField("username", librespot.ObfuscateUsername(currentPlayer.sess.Username())).
						Debugf("restored session after logout")
				}
			}
		}
	}()

	return z.Serve(func(req zeroconf.NewUserRequest) bool {
		if currentPlayer != nil {
			currentPlayer.Close()
			currentPlayer = nil

			// close the channel after setting the current session to nil
			close(apiCh)

			// no need to unset the zeroconf user here as the new one will overwrite it anyway
		}

		newAppPlayer, err := app.newAppPlayer(ctx, session.BlobCredentials{
			Username: req.Username,
			Blob:     req.AuthBlob,
		})
		if err != nil {
			app.log.WithError(err).WithField("username", librespot.ObfuscateUsername(req.Username)).
				Errorf("failed creating new session from %s", req.DeviceName)
			return false
		}

		// first create the channel and then assign the current session
		apiCh = make(chan ApiRequest)
		currentPlayer = newAppPlayer

		if app.cfg.Credentials.Zeroconf.PersistCredentials {
			app.state.Credentials.Username = newAppPlayer.sess.Username()
			app.state.Credentials.Data = newAppPlayer.sess.StoredCredentials()

			if err := app.state.Write(); err != nil {
				log.WithError(err).Errorf("failed persisting zeroconf credentials")
			}

			app.log.WithField("username", librespot.ObfuscateUsername(newAppPlayer.sess.Username())).
				Debugf("persisted zeroconf credentials")
		}

		go newAppPlayer.Run(ctx, apiCh, app.mpris.Receive())
		return true
	})
}

type Config struct {
	ConfigDir string `koanf:"config_dir"`

	// We need to keep this object around, otherwise it gets GC'd and the
	// finalizer will run, probably closing the lock.
	configLock *flock.Flock

	LogLevel                      log.Level `koanf:"log_level"`
	LogDisableTimestamp           bool      `koanf:"log_disable_timestamp"`
	DeviceId                      string    `koanf:"device_id"`
	DeviceName                    string    `koanf:"device_name"`
	DeviceType                    string    `koanf:"device_type"`
	ClientToken                   string    `koanf:"client_token"`
	AudioBackend                  string    `koanf:"audio_backend"`
	AudioDevice                   string    `koanf:"audio_device"`
	MixerDevice                   string    `koanf:"mixer_device"`
	MixerControlName              string    `koanf:"mixer_control_name"`
	AudioBufferTime               int       `koanf:"audio_buffer_time"`
	AudioPeriodCount              int       `koanf:"audio_period_count"`
	AudioOutputPipe               string    `koanf:"audio_output_pipe"`
	AudioOutputPipeFormat         string    `koanf:"audio_output_pipe_format"`
	Bitrate                       int       `koanf:"bitrate"`
	VolumeSteps                   uint32    `koanf:"volume_steps"`
	InitialVolume                 uint32    `koanf:"initial_volume"`
	IgnoreLastVolume              bool      `koanf:"ignore_last_volume"`
	NormalisationDisabled         bool      `koanf:"normalisation_disabled"`
	NormalisationUseAlbumGain     bool      `koanf:"normalisation_use_album_gain"`
	NormalisationPregain          float32   `koanf:"normalisation_pregain"`
	ExternalVolume                bool      `koanf:"external_volume"`
	ZeroconfEnabled               bool      `koanf:"zeroconf_enabled"`
	ZeroconfPort                  int       `koanf:"zeroconf_port"`
	ZeroconfBackend               string    `koanf:"zeroconf_backend"`
	DisableAutoplay               bool      `koanf:"disable_autoplay"`
	ZeroconfInterfacesToAdvertise []string  `koanf:"zeroconf_interfaces_to_advertise"`
	MprisEnabled                  bool      `koanf:"mpris_enabled"`
	FlacEnabled                   bool      `koanf:"flac_enabled"`
	Server                        struct {
		Enabled     bool   `koanf:"enabled"`
		Address     string `koanf:"address"`
		Port        int    `koanf:"port"`
		AllowOrigin string `koanf:"allow_origin"`
		CertFile    string `koanf:"cert_file"`
		KeyFile     string `koanf:"key_file"`

		ImageSize string `koanf:"image_size"`
	} `koanf:"server"`
	Credentials struct {
		Type        string `koanf:"type"`
		Interactive struct {
			CallbackPort int `koanf:"callback_port"`
		} `koanf:"interactive"`
		SpotifyToken struct {
			Username string `koanf:"username"`
			Data     string `koanf:"data"`
		} `koanf:"spotify_token"`
		Zeroconf struct {
			PersistCredentials bool `koanf:"persist_credentials"`
		} `koanf:"zeroconf"`
	} `koanf:"credentials"`
}

func loadConfig(cfg *Config) error {
	f := flag.NewFlagSet("config", flag.ContinueOnError)
	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		os.Exit(0)
	}
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	defaultConfigDir := filepath.Join(userConfigDir, "go-librespot")
	f.StringVar(&cfg.ConfigDir, "config_dir", defaultConfigDir, "the configuration directory")

	var configOverrides []string
	f.StringArrayVarP(&configOverrides, "conf", "c", nil, "override config values (format: field=value, use field1.field2=value for nested fields)")

	err = f.Parse(os.Args[1:])
	if err != nil {
		return err
	}

	// Make config directory if needed.
	err = os.MkdirAll(cfg.ConfigDir, 0o700)
	if err != nil {
		return fmt.Errorf("failed creating config directory: %w", err)
	}

	// Lock the config directory (to ensure multiple instances won't clobber
	// each others state).
	lockFilePath := filepath.Join(cfg.ConfigDir, "lockfile")
	cfg.configLock = flock.New(lockFilePath)
	if locked, err := cfg.configLock.TryLock(); err != nil {
		return fmt.Errorf("could not lock config directory: %w", err)
	} else if !locked {
		// Lock already taken! Looks like go-librespot is already running.
		return fmt.Errorf("%w (lockfile: %s)", errAlreadyRunning, lockFilePath)
	}

	k := koanf.New(".")

	// load default configuration
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"log_level": log.InfoLevel,

		"device_type": "computer",
		"bitrate":     160,

		"audio_backend":            "alsa",
		"audio_device":             "default",
		"audio_output_pipe_format": "s16le",
		"mixer_control_name":       "Master",

		"volume_steps":   100,
		"initial_volume": 100,

		"credentials.type": "zeroconf",

		"zeroconf_backend": "builtin",

		"server.address":    "localhost",
		"server.image_size": "default",
	}, "."), nil)

	// load file configuration (if available)
	var configPath string
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "config.yaml")); os.IsNotExist(err) {
		configPath = filepath.Join(cfg.ConfigDir, "config.yml")
	} else {
		configPath = filepath.Join(cfg.ConfigDir, "config.yaml")
	}

	if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed reading configuration file: %w", err)
		}
	}

	// load command line configuration
	if err := k.Load(posflag.Provider(f, ".", k), nil); err != nil {
		return fmt.Errorf("failed loading command line configuration: %w", err)
	}

	// apply command line config overrides (-c/--conf flags)
	if len(configOverrides) > 0 {
		overrideMap := make(map[string]interface{})
		for _, override := range configOverrides {
			parts := strings.SplitN(override, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid config override format: %s (expected field=value)", override)
			}
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key == "" {
				return fmt.Errorf("invalid config override: empty field name in %s", override)
			}
			overrideMap[key] = value
		}
		if err := k.Load(confmap.Provider(overrideMap, "."), nil); err != nil {
			return fmt.Errorf("failed loading config overrides: %w", err)
		}
	}

	// unmarshal configuration
	if err := k.Unmarshal("", &cfg); err != nil {
		return fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	if cfg.DeviceName == "" {
		cfg.DeviceName = "go-librespot"

		hostname, _ := os.Hostname()
		if hostname != "" {
			cfg.DeviceName += " " + hostname
		}
	}

	return nil
}

func main() {
	rand.Seed(uint64(time.Now().UnixNano()))

	var cfg Config
	if err := loadConfig(&cfg); err != nil {
		if errors.Is(err, errAlreadyRunning) {
			// Print a nice error message instead of a harder-to-read log message.
			_, _ = fmt.Fprintln(os.Stderr, "could not start:", err)
			os.Exit(1)
		}
		log.WithError(err).Fatal("failed loading config")
	}

	// set log level
	log.SetLevel(cfg.LogLevel)

	log.Infof("running go-librespot %s", librespot.VersionNumberString())

	// create new app
	app, err := NewApp(&cfg)
	if err != nil {
		log.WithError(err).Fatal("failed creating app")
	}

	// create api server if needed
	if cfg.Server.Enabled {
		app.server, err = NewApiServer(app.log, cfg.Server.Address, cfg.Server.Port, cfg.Server.AllowOrigin, cfg.Server.CertFile, cfg.Server.KeyFile)
		if err != nil {
			log.WithError(err).Fatal("failed creating api server")
		}
	} else {
		app.server, _ = NewStubApiServer(app.log)
	}

	// create mpris server if needed
	if cfg.MprisEnabled {
		app.mpris, err = mpris.NewServer(app.log)
		if err != nil {
			log.WithError(err).Fatal("failed creating mpris server")
		}
	} else {
		app.mpris = mpris.DummyServer{}
	}

	ctx := context.TODO()

	switch cfg.Credentials.Type {
	case "zeroconf":
		// ensure zeroconf is enabled
		app.cfg.ZeroconfEnabled = true
		if err := app.Zeroconf(ctx); err != nil {
			log.WithError(err).Fatal("failed running zeroconf")
		}
	case "interactive":
		if err := app.Interactive(ctx, cfg.Credentials.Interactive.CallbackPort); err != nil {
			log.WithError(err).Fatal("failed running with interactive auth")
		}
	case "spotify_token":
		if err := app.SpotifyToken(ctx, cfg.Credentials.SpotifyToken.Username, cfg.Credentials.SpotifyToken.Data); err != nil {
			log.WithError(err).Fatal("failed running with username and spotify token")
		}
	default:
		log.Fatalf("unknown credentials: %s", cfg.Credentials.Type)
	}
}
