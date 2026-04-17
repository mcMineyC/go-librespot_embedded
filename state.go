package go_librespot

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
)

type AppState struct {
	sync.Mutex

	log  Logger
	path string

	DeviceId     string          `json:"device_id"`
	EventManager json.RawMessage `json:"event_manager"`
	Credentials  struct {
		Username string `json:"username"`
		Data     []byte `json:"data"`
	} `json:"credentials"`
	WebAPICredentials struct {
		AccessToken  string `json:"access_token,omitempty"`
		RefreshToken string `json:"refresh_token,omitempty"`
		TokenType    string `json:"token_type,omitempty"`
		ExpiresAt    int64  `json:"expires_at,omitempty"`
		Scope        string `json:"scope,omitempty"`
		ClientID     string `json:"client_id,omitempty"`
		ClientSecret string `json:"client_secret,omitempty"`
	} `json:"web_api_credentials"`
	LastVolume *uint32 `json:"last_volume"`
}

func (s *AppState) SetLogger(log Logger) {
	s.log = log
}

func (s *AppState) Read(configDir string) error {
	// Keep the config path for compatibility, but do not perform any file I/O.
	// Persistence is the responsibility of the embedding app; the AppState will
	// receive state via LoadFromJSON()/DumpToJSON() instead of reading from disk.
	s.path = filepath.Join(configDir, "state.json")

	if s.log != nil {
		s.log.Debugf("app state file I/O disabled: host is responsible for providing persisted state")
	}
	return nil
}

// LoadFromJSON replaces the previous disk-based Read semantics.
// The embedding application should call LoadFromJSON with the bytes it has
// persisted (for example, read from a file or secure storage) to populate
// the in-memory state.
func (s *AppState) LoadFromJSON(data []byte) error {
	s.Lock()
	defer s.Unlock()

	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("failed unmarshalling state json: %w", err)
	}

	if s.log != nil {
		s.log.Debugf("app state loaded from provided JSON")
	}
	return nil
}

// DumpToJSON returns the JSON representation of the current in-memory state.
// The embedding app can call this method and persist the returned bytes to disk
// or to any secure storage it prefers.
func (s *AppState) DumpToJSON() ([]byte, error) {
	s.Lock()
	defer s.Unlock()

	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling state json: %w", err)
	}
	return b, nil
}

func (s *AppState) Write() error {
	// Disable automatic disk writes from the library. The embedding application
	// owns persistence and should use DumpToJSON() to obtain the state bytes,
	// then store them where appropriate.
	if s.log != nil {
		s.log.Debugf("app state Write() disabled: host is responsible for persisting state")
	}
	return nil
}
