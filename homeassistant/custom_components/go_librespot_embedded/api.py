"""Thin async REST + WebSocket client for the go-librespot embedded daemon.

Matches the routes in daemoncore/api_server.go (this fork's api-spec.yml plus
the /library, /devices, /player/transfer, /player/queue, /player/remove_from_queue
additions). No bridge process, no Python-side Spotify auth: everything goes
straight to the daemon's HTTP/WS API.
"""
from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from typing import Any

import aiohttp

_LOGGER = logging.getLogger(__name__)


class GoLibrespotApiError(Exception):
    """Raised when the daemon returns an unexpected status/response."""


class GoLibrespotClient:
    """REST client + persistent WebSocket event listener."""

    def __init__(
        self,
        session: aiohttp.ClientSession,
        host: str,
        port: int,
    ) -> None:
        self._session = session
        self._base = f"http://{host}:{port}"
        self._ws_url = f"ws://{host}:{port}/events"
        self._ws_task: asyncio.Task | None = None
        self._closing = False
        self._event_callback: Callable[[str, Any], Awaitable[None]] | None = None

    # ------------------------------------------------------------------
    # REST helpers
    # ------------------------------------------------------------------

    async def _get(self, path: str, params: dict | None = None) -> Any:
        async with self._session.get(f"{self._base}{path}", params=params) as resp:
            if resp.status == 204:
                return None
            if resp.status >= 400:
                raise GoLibrespotApiError(f"GET {path} -> {resp.status}")
            if resp.content_type == "application/json":
                return await resp.json()
            return await resp.text()

    async def _post(self, path: str, payload: dict | None = None) -> Any:
        async with self._session.post(f"{self._base}{path}", json=payload or {}) as resp:
            if resp.status == 204:
                return None
            if resp.status >= 400:
                raise GoLibrespotApiError(f"POST {path} -> {resp.status}")
            if resp.content_type == "application/json":
                return await resp.json()
            return None

    # ------------------------------------------------------------------
    # Status / playback
    # ------------------------------------------------------------------

    async def get_status(self) -> dict:
        return await self._get("/status") or {}

    async def play(self, uri: str, skip_to_uri: str = "", paused: bool = False) -> None:
        await self._post("/player/play", {"uri": uri, "skip_to_uri": skip_to_uri, "paused": paused})

    async def resume(self) -> None:
        await self._post("/player/resume")

    async def pause(self) -> None:
        await self._post("/player/pause")

    async def playpause(self) -> None:
        await self._post("/player/playpause")

    async def next(self, uri: str | None = None) -> None:
        await self._post("/player/next", {"uri": uri} if uri else {})

    async def prev(self) -> None:
        await self._post("/player/prev")

    async def seek(self, position_ms: int, relative: bool = False) -> None:
        await self._post("/player/seek", {"position": position_ms, "relative": relative})

    async def get_volume(self) -> dict:
        return await self._get("/player/volume") or {}

    async def set_volume(self, volume: int, relative: bool = False) -> None:
        await self._post("/player/volume", {"volume": volume, "relative": relative})

    async def set_repeat_context(self, enabled: bool) -> None:
        await self._post("/player/repeat_context", {"repeat_context": enabled})

    async def set_repeat_track(self, enabled: bool) -> None:
        await self._post("/player/repeat_track", {"repeat_track": enabled})

    async def set_shuffle_context(self, enabled: bool) -> None:
        await self._post("/player/shuffle_context", {"shuffle_context": enabled})

    async def add_to_queue(self, uri: str) -> None:
        await self._post("/player/add_to_queue", {"uri": uri})

    async def remove_from_queue(self, uri: str) -> None:
        await self._post("/player/remove_from_queue", {"uri": uri})

    async def get_queue(self) -> dict:
        return await self._get("/player/queue") or {}

    # ------------------------------------------------------------------
    # Browse / search / devices (this fork's additions)
    # ------------------------------------------------------------------

    async def get_library(self) -> list[dict]:
        return await self._get("/library") or []

    async def search(self, query: str, limit: int = 20) -> list[dict]:
        return await self._get("/search", {"q": query, "limit": limit}) or []

    async def get_devices(self) -> list[dict]:
        return await self._get("/devices") or []

    async def transfer_playback(self, device_id: str, play: bool = True) -> None:
        await self._post("/player/transfer", {"device_id": device_id, "play": play})

    # ------------------------------------------------------------------
    # WebSocket event stream
    # ------------------------------------------------------------------

    def set_event_callback(self, callback: Callable[[str, Any], Awaitable[None]]) -> None:
        self._event_callback = callback

    def start(self) -> None:
        if self._ws_task is None or self._ws_task.done():
            self._closing = False
            self._ws_task = asyncio.create_task(self._ws_loop())

    async def stop(self) -> None:
        self._closing = True
        if self._ws_task:
            self._ws_task.cancel()
            try:
                await self._ws_task
            except asyncio.CancelledError:
                pass

    async def _ws_loop(self) -> None:
        from .const import WS_RECONNECT_DELAY

        while not self._closing:
            try:
                async with self._session.ws_connect(self._ws_url, heartbeat=30) as ws:
                    _LOGGER.debug("connected to %s", self._ws_url)
                    async for msg in ws:
                        if msg.type != aiohttp.WSMsgType.TEXT:
                            continue
                        try:
                            data = msg.json()
                        except ValueError:
                            continue
                        ev_type = data.get("type")
                        ev_data = data.get("data")
                        if self._event_callback and ev_type:
                            await self._event_callback(ev_type, ev_data)
            except asyncio.CancelledError:
                raise
            except Exception as err:  # noqa: BLE001
                _LOGGER.debug("websocket error, reconnecting: %s", err)

            if not self._closing:
                await asyncio.sleep(WS_RECONNECT_DELAY)
