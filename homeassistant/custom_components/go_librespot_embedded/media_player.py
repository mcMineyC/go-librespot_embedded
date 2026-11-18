"""Media player entity for go-librespot (embedded fork)."""
from __future__ import annotations

import logging
from typing import Any

from homeassistant.components.media_player import (
    BrowseMedia,
    MediaClass,
    MediaPlayerEntity,
    MediaPlayerEntityFeature,
    MediaPlayerState,
    MediaType,
)
from homeassistant.config_entries import ConfigEntry
from homeassistant.const import CONF_HOST, CONF_NAME, CONF_PORT
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from .api import GoLibrespotApiError, GoLibrespotClient
from .const import DEFAULT_NAME, DOMAIN

_LOGGER = logging.getLogger(__name__)

SUPPORTED_FEATURES = (
    MediaPlayerEntityFeature.PLAY
    | MediaPlayerEntityFeature.PAUSE
    | MediaPlayerEntityFeature.STOP
    | MediaPlayerEntityFeature.NEXT_TRACK
    | MediaPlayerEntityFeature.PREVIOUS_TRACK
    | MediaPlayerEntityFeature.SEEK
    | MediaPlayerEntityFeature.VOLUME_SET
    | MediaPlayerEntityFeature.SHUFFLE_SET
    | MediaPlayerEntityFeature.REPEAT_SET
    | MediaPlayerEntityFeature.PLAY_MEDIA
    | MediaPlayerEntityFeature.BROWSE_MEDIA
    | MediaPlayerEntityFeature.SEARCH_MEDIA
    | MediaPlayerEntityFeature.SELECT_SOURCE
)

# go-librespot "kind" -> HA media_class, for browse_media icons/grouping
KIND_TO_MEDIA_CLASS = {
    "playlist": MediaClass.PLAYLIST,
    "album": MediaClass.ALBUM,
    "artist": MediaClass.ARTIST,
    "show": MediaClass.PODCAST,
    "folder": MediaClass.DIRECTORY,
    "collection": MediaClass.PLAYLIST,
}


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    data = hass.data[DOMAIN][entry.entry_id]
    client: GoLibrespotClient = data["client"]
    name = entry.data.get(CONF_NAME, DEFAULT_NAME)
    async_add_entities([GoLibrespotMediaPlayer(entry.entry_id, client, name)])


class GoLibrespotMediaPlayer(MediaPlayerEntity):
    """Represents the go-librespot daemon as a Spotify Connect media player."""

    _attr_supported_features = SUPPORTED_FEATURES
    _attr_should_poll = False

    def __init__(self, entry_id: str, client: GoLibrespotClient, name: str) -> None:
        self._client = client
        self._attr_name = name
        self._attr_unique_id = f"{entry_id}_media_player"

        self._state = MediaPlayerState.IDLE
        self._volume: float | None = None
        self._volume_max = 100
        self._shuffle = False
        self._repeat_context = False
        self._repeat_track = False

        self._media_title: str | None = None
        self._media_artist: str | None = None
        self._media_album: str | None = None
        self._media_image_url: str | None = None
        self._media_duration: int | None = None
        self._media_position: int | None = None
        self._media_uri: str | None = None
        self._context_uri: str | None = None

        self._devices: list[dict] = []

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def async_added_to_hass(self) -> None:
        self._client.set_event_callback(self._handle_event)
        self._client.start()
        await self._refresh_status()
        await self._refresh_devices()

    async def async_will_remove_from_hass(self) -> None:
        await self._client.stop()

    async def _refresh_status(self) -> None:
        try:
            status = await self._client.get_status()
        except GoLibrespotApiError:
            self._state = MediaPlayerState.OFF
            self.async_write_ha_state()
            return

        self._apply_status(status)
        self.async_write_ha_state()

    async def _refresh_devices(self) -> None:
        try:
            self._devices = await self._client.get_devices()
        except GoLibrespotApiError:
            self._devices = []
        self.async_write_ha_state()

    def _apply_status(self, status: dict) -> None:
        if status.get("stopped"):
            self._state = MediaPlayerState.IDLE
        elif status.get("paused"):
            self._state = MediaPlayerState.PAUSED
        else:
            self._state = MediaPlayerState.PLAYING

        steps = status.get("volume_steps") or self._volume_max
        self._volume_max = steps or 100
        vol = status.get("volume")
        if vol is not None and self._volume_max:
            self._volume = vol / self._volume_max

        self._shuffle = bool(status.get("shuffle_context"))
        self._repeat_context = bool(status.get("repeat_context"))
        self._repeat_track = bool(status.get("repeat_track"))

        track = status.get("track") or {}
        self._apply_track(track)

    def _apply_track(self, track: dict) -> None:
        self._media_title = track.get("name")
        artists = track.get("artist_names") or []
        self._media_artist = ", ".join(artists) if artists else None
        self._media_album = track.get("album_name")
        self._media_image_url = track.get("album_cover_url")
        self._media_duration = _ms_to_s(track.get("duration"))
        self._media_position = _ms_to_s(track.get("position"))
        self._media_uri = track.get("uri")

    # ------------------------------------------------------------------
    # WebSocket event handling (instant updates, no polling)
    # ------------------------------------------------------------------

    async def _handle_event(self, ev_type: str, data: Any) -> None:
        data = data or {}

        if ev_type == "metadata":
            self._apply_track(data)
        elif ev_type in ("playing",):
            self._state = MediaPlayerState.PLAYING
            self._context_uri = data.get("context_uri")
        elif ev_type == "paused":
            self._state = MediaPlayerState.PAUSED
        elif ev_type in ("stopped", "not_playing"):
            self._state = MediaPlayerState.IDLE
        elif ev_type == "seek":
            self._media_position = _ms_to_s(data.get("position"))
            self._media_duration = _ms_to_s(data.get("duration"))
        elif ev_type == "volume":
            max_v = data.get("max") or self._volume_max
            self._volume_max = max_v or 100
            val = data.get("value")
            if val is not None and self._volume_max:
                self._volume = val / self._volume_max
        elif ev_type == "shuffle_context":
            self._shuffle = bool(data.get("value"))
        elif ev_type == "repeat_context":
            self._repeat_context = bool(data.get("value"))
        elif ev_type == "repeat_track":
            self._repeat_track = bool(data.get("value"))
        elif ev_type == "active":
            await self._refresh_devices()

        self.async_write_ha_state()

    # ------------------------------------------------------------------
    # State properties
    # ------------------------------------------------------------------

    @property
    def state(self) -> MediaPlayerState:
        return self._state

    @property
    def volume_level(self) -> float | None:
        return self._volume

    @property
    def shuffle(self) -> bool:
        return self._shuffle

    @property
    def repeat(self) -> str:
        if self._repeat_track:
            return "one"
        if self._repeat_context:
            return "all"
        return "off"

    @property
    def media_title(self) -> str | None:
        return self._media_title

    @property
    def media_artist(self) -> str | None:
        return self._media_artist

    @property
    def media_album_name(self) -> str | None:
        return self._media_album

    @property
    def media_image_url(self) -> str | None:
        return self._media_image_url

    @property
    def media_duration(self) -> int | None:
        return self._media_duration

    @property
    def media_position(self) -> int | None:
        return self._media_position

    @property
    def media_content_id(self) -> str | None:
        return self._media_uri

    @property
    def media_content_type(self) -> str | None:
        return MediaType.MUSIC

    @property
    def source_list(self) -> list[str]:
        return [d["name"] for d in self._devices]

    @property
    def source(self) -> str | None:
        for d in self._devices:
            if d.get("is_active"):
                return d.get("name")
        return None

    # ------------------------------------------------------------------
    # Standard playback commands
    # ------------------------------------------------------------------

    async def async_media_play(self) -> None:
        await self._client.resume()

    async def async_media_pause(self) -> None:
        await self._client.pause()

    async def async_media_play_pause(self) -> None:
        await self._client.playpause()

    async def async_media_stop(self) -> None:
        await self._client.pause()

    async def async_media_next_track(self) -> None:
        await self._client.next()

    async def async_media_previous_track(self) -> None:
        await self._client.prev()

    async def async_media_seek(self, position: float) -> None:
        await self._client.seek(int(position * 1000))

    async def async_set_volume_level(self, volume: float) -> None:
        await self._client.set_volume(round(volume * self._volume_max))

    async def async_set_shuffle(self, shuffle: bool) -> None:
        await self._client.set_shuffle_context(shuffle)

    async def async_set_repeat(self, repeat: str) -> None:
        if repeat == "one":
            await self._client.set_repeat_context(True)
            await self._client.set_repeat_track(True)
        elif repeat == "all":
            await self._client.set_repeat_context(True)
            await self._client.set_repeat_track(False)
        else:
            await self._client.set_repeat_context(False)
            await self._client.set_repeat_track(False)

    async def async_play_media(self, media_type: str, media_id: str, **kwargs: Any) -> None:
        """Play a Spotify URI. media_id is expected to be e.g. spotify:playlist:...

        Also handles our synthetic search-result ids ("search:<spotify_id>"),
        produced by async_search_media below.
        """
        uri = media_id
        if uri.startswith("search:"):
            uri = uri[len("search:"):]
            if not uri.startswith("spotify:"):
                uri = f"spotify:track:{uri}"
        await self._client.play(uri)

    # ------------------------------------------------------------------
    # Device select ("source") -> Spotify Connect transfer
    # ------------------------------------------------------------------

    async def async_select_source(self, source: str) -> None:
        for d in self._devices:
            if d.get("name") == source:
                await self._client.transfer_playback(d["id"], play=True)
                return
        _LOGGER.warning("unknown source/device: %s", source)

    # ------------------------------------------------------------------
    # Library browse
    # ------------------------------------------------------------------

    async def async_browse_media(
        self, media_content_type: str | None = None, media_content_id: str | None = None
    ) -> BrowseMedia:
        # This daemon only exposes the rootlist (playlists/albums/shows/etc
        # the user has saved), not per-item track listings — there's no
        # /playlist/{uri}/tracks endpoint. Selecting a rootlist item plays
        # its whole context via /player/play, same model as the official
        # Spotify integration's non-expandable browse nodes.
        try:
            items = await self._client.get_library()
        except GoLibrespotApiError:
            items = []

        children = [
            BrowseMedia(
                title=item.get("name", item.get("uri", "Unknown")),
                media_class=KIND_TO_MEDIA_CLASS.get(item.get("kind"), MediaClass.TRACK),
                media_content_id=item["uri"],
                media_content_type=MediaType.MUSIC,
                can_play=True,
                can_expand=False,
                thumbnail=item.get("image_url"),
            )
            for item in items
            if item.get("uri")
        ]

        return BrowseMedia(
            title="Your Library",
            media_class=MediaClass.DIRECTORY,
            media_content_id="library",
            media_content_type=MediaType.MUSIC,
            can_play=False,
            can_expand=True,
            children=children,
        )

    async def async_search_media(self, query) -> Any:
        from homeassistant.components.media_player import SearchMedia, SearchMediaQuery  # noqa: F401

        try:
            results = await self._client.search(query.search_query, limit=20)
        except GoLibrespotApiError:
            results = []

        items = [
            BrowseMedia(
                title=r.get("title", r.get("spotify_id", "Unknown")),
                media_class=MediaClass.TRACK,
                media_content_id=f"search:{r.get('spotify_id')}",
                media_content_type=MediaType.MUSIC,
                can_play=True,
                can_expand=False,
            )
            for r in results
            if r.get("spotify_id")
        ]
        return type("Result", (), {"result": items})()


def _ms_to_s(value: int | None) -> int | None:
    if value is None:
        return None
    return int(value / 1000)
