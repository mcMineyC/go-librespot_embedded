package spclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v4"
	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	storagepb "github.com/devgianlu/go-librespot/proto/spotify/download"
	eventsenderpb "github.com/devgianlu/go-librespot/proto/spotify/event_sender"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	netfortunepb "github.com/devgianlu/go-librespot/proto/spotify/netfortune"
	playerpb "github.com/devgianlu/go-librespot/proto/spotify/player"
	playlist4pb "github.com/devgianlu/go-librespot/proto/spotify/playlist4"
	streamingpb "github.com/devgianlu/go-librespot/proto/spotify/streaming"
	"google.golang.org/protobuf/proto"
)

type Spclient struct {
	log librespot.Logger

	client *http.Client

	baseUrl     *url.URL
	clientToken string
	deviceId    string

	accessToken librespot.GetLogin5TokenFunc
}

func NewSpclient(ctx context.Context, log librespot.Logger, client *http.Client, addr librespot.GetAddressFunc, accessToken librespot.GetLogin5TokenFunc, deviceId, clientToken string) (*Spclient, error) {
	baseUrl, err := url.Parse(fmt.Sprintf("https://%s/", addr(ctx)))
	if err != nil {
		return nil, fmt.Errorf("invalid spclient base url: %w", err)
	}

	return &Spclient{
		log:         log,
		client:      client,
		baseUrl:     baseUrl,
		clientToken: clientToken,
		deviceId:    deviceId,
		accessToken: accessToken,
	}, nil
}

func (c *Spclient) innerRequest(ctx context.Context, method string, reqUrl *url.URL, query url.Values, header http.Header, body []byte) (*http.Response, error) {
	if query != nil {
		reqUrl.RawQuery = query.Encode()
	}

	req := &http.Request{
		URL:    reqUrl,
		Method: method,
		Header: http.Header{},
	}

	if header != nil {
		for name, values := range header {
			req.Header[name] = values
		}
	}

	req.Header.Set("Client-Token", c.clientToken)

	if body != nil {
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-protobuf")
		}

		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		req.Body, _ = req.GetBody()
	}

	var forceNewToken bool
	resp, err := backoff.RetryWithData(func() (*http.Response, error) {
		accessToken, err := c.accessToken(ctx, forceNewToken)
		if err != nil {
			// Fail with a permanent error if we can't get a new token. The caller should have already retried, there's
			// nothing we can do.
			return nil, backoff.Permanent(fmt.Errorf("failed obtaining spclient access token: %w", err))
		}

		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

		resp, err := c.client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		} else if resp.StatusCode == 401 {
			_ = resp.Body.Close()

			forceNewToken = true
			return nil, fmt.Errorf("unauthorized")
		} else if resp.StatusCode == 502 {
			_ = resp.Body.Close()

			c.log.Debugf("spclient request returned bad gateway, retrying...")
			return nil, fmt.Errorf("bad gateway")
		}

		return resp, nil
	}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
	if err != nil {
		return nil, fmt.Errorf("spclient request failed: %w", err)
	}

	return resp, nil
}

func (c *Spclient) WebApiRequest(ctx context.Context, method string, path string, query url.Values, header http.Header, body []byte) (*http.Response, error) {
	reqPath, err := url.Parse("https://api.spotify.com/")
	if err != nil {
		panic("invalid api base url")
	}
	reqURL := reqPath.JoinPath(path)
	return c.innerRequest(ctx, method, reqURL, query, header, body)
}

func (c *Spclient) Request(ctx context.Context, method string, path string, query url.Values, header http.Header, body []byte) (*http.Response, error) {
	reqUrl := c.baseUrl.JoinPath(path)
	return c.innerRequest(ctx, method, reqUrl, query, header, body)
}

type putStateError struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
}

func (c *Spclient) PutConnectStateInactive(ctx context.Context, spotConnId string, notify bool) error {
	resp, err := c.Request(
		ctx,
		"PUT",
		fmt.Sprintf("/connect-state/v1/devices/%s/inactive", c.deviceId),
		url.Values{"notify": []string{strconv.FormatBool(notify)}},
		http.Header{
			"X-Spotify-Connection-Id": []string{spotConnId},
		},
		nil,
	)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 204 {
		return fmt.Errorf("put state inactive request failed with status %d", resp.StatusCode)
	} else {
		c.log.Debug("put connect state inactive")
		return nil
	}
}

func (c *Spclient) PutConnectState(ctx context.Context, spotConnId string, reqProto *connectpb.PutStateRequest) error {
	reqBody, err := proto.Marshal(reqProto)
	if err != nil {
		return fmt.Errorf("failed marshalling PutStateRequest: %w", err)
	}
	_, err = backoff.RetryWithData(func() (*http.Response, error) {
		resp, err := c.Request(
			ctx,
			"PUT",
			fmt.Sprintf("/connect-state/v1/devices/%s", c.deviceId),
			nil,
			http.Header{
				"X-Spotify-Connection-Id": []string{spotConnId},
				"Content-Type":            []string{"application/x-protobuf"},
			},
			reqBody,
		)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != 200 {
			var putError putStateError
			if err := json.NewDecoder(resp.Body).Decode(&putError); err != nil {
				c.log.Debugf("failed reading error response %s", err)
				return nil, fmt.Errorf("failed reading error response: %w", err)
			}
			c.log.Debugf("put state request failed with status %d: %s", resp.StatusCode, putError.Message)
			return nil, fmt.Errorf("put state request failed with status %d: %s", resp.StatusCode, putError.Message)
		} else {
			c.log.Debugf("put connect state because %s", reqProto.PutStateReason)
			return resp, nil
		}
	}, backoff.WithContext(backoff.WithMaxRetries(backoff.NewConstantBackOff(1*time.Second), 2), ctx))
	if err != nil {
		return err
	}
	return nil
}

// PutConnectStateWithCluster is like PutConnectState but also returns the raw
// response body, which is a serialised Cluster protobuf. Used by daemoncore to
// seed the cluster cache on the initial NEW_DEVICE put.
func (c *Spclient) PutConnectStateWithCluster(ctx context.Context, spotConnId string, reqProto *connectpb.PutStateRequest) ([]byte, error) {
	reqBody, err := proto.Marshal(reqProto)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling PutStateRequest: %w", err)
	}
	body, err := backoff.RetryWithData(func() ([]byte, error) {
		resp, err := c.Request(
			ctx,
			"PUT",
			fmt.Sprintf("/connect-state/v1/devices/%s", c.deviceId),
			nil,
			http.Header{
				"X-Spotify-Connection-Id": []string{spotConnId},
				"Content-Type":            []string{"application/x-protobuf"},
			},
			reqBody,
		)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()

		respBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("failed reading PutConnectState response: %w", readErr)
		}

		if resp.StatusCode != 200 {
			var putError putStateError
			if decErr := json.Unmarshal(respBytes, &putError); decErr == nil {
				c.log.Debugf("put state request failed with status %d: %s", resp.StatusCode, putError.Message)
				return nil, fmt.Errorf("put state request failed with status %d: %s", resp.StatusCode, putError.Message)
			}
			return nil, fmt.Errorf("put state request failed with status %d", resp.StatusCode)
		}

		c.log.Debugf("put connect state because %s", reqProto.PutStateReason)
		return respBytes, nil
	}, backoff.WithContext(backoff.WithMaxRetries(backoff.NewConstantBackOff(1*time.Second), 2), ctx))
	return body, err
}

func (c *Spclient) ResolveStorageInteractive(ctx context.Context, fileId []byte, format *metadatapb.AudioFile_Format, prefetch bool) (*storagepb.StorageResolveResponse, error) {
	var path string
	if prefetch {
		path = fmt.Sprintf("/storage-resolve/v2/files/audio/interactive_prefetch/%d/%s", format.Number(), hex.EncodeToString(fileId))
	} else {
		path = fmt.Sprintf("/storage-resolve/v2/files/audio/interactive/%d/%s", format.Number(), hex.EncodeToString(fileId))
	}

	resp, err := c.Request(ctx, "GET", path, nil, nil, nil)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 503 {
		c.log.Debugf("storage resolve returned service unavailable, retrying...")
		_ = resp.Body.Close()

		resp, err = c.Request(ctx, "GET", path, nil, nil, nil)
		if err != nil {
			return nil, err
		}
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from storage resolve: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var protoResp storagepb.StorageResolveResponse
	if err := proto.Unmarshal(respBytes, &protoResp); err != nil {
		return nil, fmt.Errorf("failed unmarshalling StorageResolveResponse: %w", err)
	}

	return &protoResp, nil
}

func (c *Spclient) ExtendedMetadata(ctx context.Context, req *extmetadatapb.BatchedEntityRequest) (*extmetadatapb.BatchedExtensionResponse, error) {
	reqBody, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling BatchedEntityRequest: %w", err)
	}

	resp, err := c.Request(ctx, "POST", "/extended-metadata/v0/extended-metadata", nil, nil, reqBody)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from extended metadata: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var protoResp extmetadatapb.BatchedExtensionResponse
	if err := proto.Unmarshal(respBytes, &protoResp); err != nil {
		return nil, fmt.Errorf("failed unmarshalling BatchedExtensionResponse: %w", err)
	}

	return &protoResp, nil
}

func (c *Spclient) ExtendedMetadataSimple(ctx context.Context, id librespot.SpotifyId, ext extmetadatapb.ExtensionKind, data proto.Message) error {
	resp, err := c.ExtendedMetadata(ctx, &extmetadatapb.BatchedEntityRequest{
		EntityRequest: []*extmetadatapb.EntityRequest{{
			EntityUri: id.Uri(),
			Query: []*extmetadatapb.ExtensionQuery{{
				ExtensionKind: ext,
			}},
		}},
	})
	if err != nil {
		return err
	}

	for _, item := range resp.ExtendedMetadata {
		if item.ExtensionKind != ext {
			continue
		}

		for _, extData := range item.ExtensionData {
			if extData.EntityUri != id.Uri() {
				continue
			}

			if extData.Header.StatusCode != 200 {
				return fmt.Errorf("extended metadata request returned status %d", extData.Header.StatusCode)
			}

			if err := extData.ExtensionData.UnmarshalTo(data); err != nil {
				return fmt.Errorf("failed unmarshalling extended metadata data: %w", err)
			}

			return nil
		}
	}

	return fmt.Errorf("extended metadata with kind %s not found", ext)
}

func (c *Spclient) PlaylistSignals(ctx context.Context, playlist librespot.SpotifyId, reqProto *playlist4pb.ListSignals, lenses []string) (*playlist4pb.SelectedListContent, error) {
	if playlist.Type() != librespot.SpotifyIdTypePlaylist {
		panic(fmt.Sprintf("invalid type: %s", playlist.Type()))
	}

	reqBody, err := proto.Marshal(reqProto)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling ListSignals: %w", err)
	}

	resp, err := c.Request(ctx, "POST", fmt.Sprintf("/playlist/v2/playlist/%s/signals", playlist.Base62()), nil, http.Header{
		"Spotify-Apply-Lenses": lenses,
	}, reqBody)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from playlist signals: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var protoResp playlist4pb.SelectedListContent
	if err := proto.Unmarshal(respBytes, &protoResp); err != nil {
		return nil, fmt.Errorf("failed unmarshalling SelectedListContent: %w", err)
	}

	return &protoResp, nil
}

func (c *Spclient) ContextResolve(ctx context.Context, uri string) (*connectpb.Context, error) {
	resp, err := c.Request(ctx, "GET", fmt.Sprintf("/context-resolve/v1/%s", uri), nil, nil, nil)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from context resolve: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var context connectpb.Context
	if err := json.Unmarshal(respBytes, &context); err != nil {
		return nil, fmt.Errorf("failed json unmarshalling Context: %w", err)
	}

	return &context, nil
}

func (c *Spclient) ContextResolveAutoplay(ctx context.Context, reqProto *playerpb.AutoplayContextRequest) (*connectpb.Context, error) {
	reqBody, err := proto.Marshal(reqProto)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling AutoplayContextRequest: %w", err)
	}

	resp, err := c.Request(ctx, "POST", "/context-resolve/v1/autoplay", nil, nil, reqBody)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from context resolve autoplay: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var context connectpb.Context
	if err := json.Unmarshal(respBytes, &context); err != nil {
		return nil, fmt.Errorf("failed json unmarshalling Context: %w", err)
	}

	return &context, nil
}

func (c *Spclient) GetAccessToken(ctx context.Context, force bool) (string, error) {
	return c.accessToken(ctx, force)
}

func (c *Spclient) NetFortune(ctx context.Context, bandwidth int) (*netfortunepb.NetFortuneV2Response, error) {
	resp, err := c.Request(ctx, "GET", "/net-fortune/v2/fortune", url.Values{
		"bandwidth": []string{fmt.Sprintf("%d", bandwidth)},
	}, nil, nil)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from net fortune: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var fortune netfortunepb.NetFortuneV2Response
	if err := proto.Unmarshal(respBytes, &fortune); err != nil {
		return nil, fmt.Errorf("failed json unmarshalling NetFortuneV2Response: %w", err)
	}

	return &fortune, nil
}

func (c *Spclient) PublishEvents(ctx context.Context, reqProto *eventsenderpb.PublishEventsRequest) (*eventsenderpb.PublishEventsResponse, error) {
	reqBody, err := proto.Marshal(reqProto)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling PublishEventsRequest: %w", err)
	}

	resp, err := c.Request(ctx, "POST", "/gabo-receiver-service/v3/events/", nil, nil, reqBody)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from gabo events receiver: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var respProto eventsenderpb.PublishEventsResponse
	if err := proto.Unmarshal(respBytes, &respProto); err != nil {
		return nil, fmt.Errorf("failed json unmarshalling PublishEventsResponse: %w", err)
	}

	return &respProto, nil
}

func (c *Spclient) PlayPlayRequest(ctx context.Context, fileId []byte, reqProto *streamingpb.PlayPlayLicenseRequest) (*streamingpb.PlayPlayLicenseResponse, error) {
	reqBody, err := proto.Marshal(reqProto)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling PlayPlayLicenseRequest: %w", err)
	}

	resp, err := c.Request(ctx, "POST", fmt.Sprintf("/playplay/v1/key/%x", fileId), nil, nil, reqBody)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from playplay license request: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var respProto streamingpb.PlayPlayLicenseResponse
	if err := proto.Unmarshal(respBytes, &respProto); err != nil {
		return nil, fmt.Errorf("failed json unmarshalling PlayPlayLicenseResponse: %w", err)
	}

	return &respProto, nil
}

// ---------------------------------------------------------------------------
// Connect-state player command / transfer / volume helpers
// (mirrors spotcontrol's spclient implementation)
// ---------------------------------------------------------------------------

// randomHex returns a random lowercase hex string of n bytes (2*n characters).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CommandLoggingParams is the logging_params object sent with connect-state
// player commands, matching the Spotify desktop client's format.
type CommandLoggingParams struct {
	CommandInitiatedTime *int64   `json:"command_initiated_time,omitempty"`
	CommandReceivedTime  *int64   `json:"command_received_time,omitempty"`
	PageInstanceIds      []string `json:"page_instance_ids"`
	InteractionIds       []string `json:"interaction_ids"`
	DeviceIdentifier     string   `json:"device_identifier,omitempty"`
	CommandId            string   `json:"command_id,omitempty"`
}

// CommandOptions contains options sent with connect-state player commands.
type CommandOptions struct {
	RestorePaused        string `json:"restore_paused,omitempty"`
	AllowSeeking         bool   `json:"allow_seeking,omitempty"`
	OverrideRestrictions bool   `json:"override_restrictions"`
	OnlyForLocalDevice   bool   `json:"only_for_local_device"`
	SystemInitiated      bool   `json:"system_initiated"`
}

// CommandSkipTo specifies which track to skip to within a play command.
type CommandSkipTo struct {
	TrackUid   string `json:"track_uid,omitempty"`
	TrackUri   string `json:"track_uri,omitempty"`
	TrackIndex int    `json:"track_index,omitempty"`
}

// PreparePlayOptions carries prepare_play_options for "play" commands.
type PreparePlayOptions struct {
	AlwaysPlaySomething   bool                   `json:"always_play_something"`
	SkipTo                *CommandSkipTo         `json:"skip_to,omitempty"`
	InitiallyPaused       bool                   `json:"initially_paused"`
	SystemInitiated       bool                   `json:"system_initiated"`
	PlayerOptionsOverride *PlayerOptionsOverride `json:"player_options_override,omitempty"`
	SessionId             string                 `json:"session_id,omitempty"`
	License               string                 `json:"license,omitempty"`
	SuppressionsObj       interface{}            `json:"suppressions,omitempty"`
	PrefetchLevel         string                 `json:"prefetch_level,omitempty"`
	AudioStream           string                 `json:"audio_stream,omitempty"`
	ConfigurationOverride interface{}            `json:"configuration_override,omitempty"`
}

// PlayerOptionsOverride carries player option overrides in PreparePlayOptions.
type PlayerOptionsOverride struct {
	ShufflingContext bool `json:"shuffling_context"`
}

// PlayOptions carries play_options for "play" commands.
type PlayOptions struct {
	Reason               string `json:"reason,omitempty"`
	Operation            string `json:"operation,omitempty"`
	Trigger              string `json:"trigger,omitempty"`
	OverrideRestrictions bool   `json:"override_restrictions"`
	OnlyForLocalDevice   bool   `json:"only_for_local_device"`
	SystemInitiated      bool   `json:"system_initiated"`
}

// PlayerCommand is the command object nested inside a PlayerCommandRequest.
type PlayerCommand struct {
	Endpoint           string                  `json:"endpoint"`
	Value              interface{}             `json:"value,omitempty"`
	Context            *connectpb.Context      `json:"context,omitempty"`
	PlayOrigin         *connectpb.PlayOrigin   `json:"play_origin,omitempty"`
	Track              *connectpb.ContextTrack `json:"track,omitempty"`
	PrevTracks         []*connectpb.ContextTrack `json:"prev_tracks,omitempty"`
	NextTracks         []*connectpb.ContextTrack `json:"next_tracks,omitempty"`
	Options            *CommandOptions         `json:"options,omitempty"`
	LoggingParams      *CommandLoggingParams   `json:"logging_params,omitempty"`
	PreparePlayOptions *PreparePlayOptions     `json:"prepare_play_options,omitempty"`
	PlayOptions        *PlayOptions            `json:"play_options,omitempty"`
}

// PlayerCommandRequest is the top-level JSON envelope for the connect-state
// player command endpoint.
type PlayerCommandRequest struct {
	Command        *PlayerCommand `json:"command"`
	ConnectionType string         `json:"connection_type,omitempty"`
	IntentId       string         `json:"intent_id,omitempty"`
}

// TransferOptions mirrors librespot/spotcontrol's TransferOptions struct.
type TransferOptions struct {
	RestorePaused   *string `json:"restore_paused,omitempty"`
	RestorePosition *string `json:"restore_position,omitempty"`
	RestoreTrack    *string `json:"restore_track,omitempty"`
	RetainSession   *string `json:"retain_session,omitempty"`
}

// TransferRequest is the JSON body for the connect-state transfer endpoint.
type TransferRequest struct {
	TransferOptions TransferOptions `json:"transfer_options"`
}

// NewLoggingParams builds a CommandLoggingParams matching the desktop client.
func (c *Spclient) NewLoggingParams() *CommandLoggingParams {
	now := time.Now().UnixMilli()
	received := now + 2
	return &CommandLoggingParams{
		CommandInitiatedTime: &now,
		CommandReceivedTime:  &received,
		PageInstanceIds:      []string{},
		InteractionIds:       []string{},
		DeviceIdentifier:     c.deviceId,
		CommandId:            randomHex(16),
	}
}

// NewCommandOptions creates the default CommandOptions matching the desktop client.
func NewCommandOptions() *CommandOptions {
	return &CommandOptions{
		OverrideRestrictions: false,
		OnlyForLocalDevice:   false,
		SystemInitiated:      false,
	}
}

// ConnectPlayerCommand sends a playback command to targetDeviceId through
// the connect-state player command endpoint:
//
//	POST /connect-state/v1/player/command/from/{fromDevice}/to/{targetDevice}
//
// The body is a gzip-compressed JSON PlayerCommandRequest, matching the
// Spotify desktop client wire format.
func (c *Spclient) ConnectPlayerCommand(
	ctx context.Context,
	spotConnId string,
	targetDeviceId string,
	cmdReq *PlayerCommandRequest,
) error {
	jsonBody, err := json.Marshal(cmdReq)
	if err != nil {
		return fmt.Errorf("failed marshalling player command request: %w", err)
	}

	// Gzip-compress the JSON body, matching the desktop client.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(jsonBody); err != nil {
		return fmt.Errorf("failed gzip compressing player command: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("failed closing gzip writer: %w", err)
	}

	path := fmt.Sprintf(
		"/connect-state/v1/player/command/from/%s/to/%s",
		c.deviceId, targetDeviceId,
	)

	resp, err := c.Request(ctx, "POST", path, nil, http.Header{
		"X-Spotify-Connection-Id": []string{spotConnId},
		"Content-Type":            []string{"application/json"},
		"Content-Encoding":        []string{"gzip"},
	}, buf.Bytes())
	if err != nil {
		return fmt.Errorf("failed sending connect player command: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case 200, 202, 204:
		return nil
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("connect player command (%s) failed with status %d: %s",
			cmdReq.Command.Endpoint, resp.StatusCode, string(respBody))
	}
}

// ConnectTransfer triggers a playback transfer via the connect-state transfer
// endpoint:
//
//	POST /connect-state/v1/connect/transfer/from/{fromDeviceId}/to/{toDeviceId}
//
// This is the same endpoint librespot uses (spclient.rs transfer()).
// Pass nil transferReq to send an empty POST body.
func (c *Spclient) ConnectTransfer(
	ctx context.Context,
	spotConnId string,
	fromDeviceId string,
	toDeviceId string,
	transferReq *TransferRequest,
) error {
	path := fmt.Sprintf(
		"/connect-state/v1/connect/transfer/from/%s/to/%s",
		fromDeviceId, toDeviceId,
	)

	var body []byte
	if transferReq != nil {
		var err error
		body, err = json.Marshal(transferReq)
		if err != nil {
			return fmt.Errorf("failed marshalling transfer request: %w", err)
		}
	}

	header := http.Header{
		"X-Spotify-Connection-Id": []string{spotConnId},
	}
	if body != nil {
		header.Set("Content-Type", "application/json")
	}

	resp, err := c.Request(ctx, "POST", path, nil, header, body)
	if err != nil {
		return fmt.Errorf("failed sending connect transfer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case 200, 202, 204:
		return nil
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("connect transfer failed with status %d: %s",
			resp.StatusCode, string(respBody))
	}
}

// ConnectSetVolume sends a volume change via the connect-state volume endpoint:
//
//	PUT /connect-state/v1/connect/volume/from/{fromDevice}/to/{targetDevice}
//
// volume is the raw connect-state value (0–65535, matching player.MaxStateVolume).
// This is the same mechanism librespot uses for SetVolumeCommand handling.
func (c *Spclient) ConnectSetVolume(
	ctx context.Context,
	spotConnId string,
	targetDeviceId string,
	volume int32,
) error {
	volumeCmd := &connectpb.SetVolumeCommand{
		Volume: volume,
	}

	body, err := proto.Marshal(volumeCmd)
	if err != nil {
		return fmt.Errorf("failed marshalling SetVolumeCommand: %w", err)
	}

	path := fmt.Sprintf(
		"/connect-state/v1/connect/volume/from/%s/to/%s",
		c.deviceId, targetDeviceId,
	)

	resp, err := c.Request(ctx, "PUT", path, nil, http.Header{
		"X-Spotify-Connection-Id": []string{spotConnId},
		"Content-Type":            []string{"application/x-protobuf"},
	}, body)
	if err != nil {
		return fmt.Errorf("failed sending connect volume: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case 200, 202, 204:
		return nil
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("connect volume command failed with status %d: %s",
			resp.StatusCode, string(respBody))
	}
}
