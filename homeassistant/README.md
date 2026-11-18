# go-librespot (embedded fork) — Home Assistant integration

Talks directly to the daemon's REST/WS API. No Python bridge, no separate
process — pure HTTP + WebSocket from the HA custom_component to your Go daemon.

## What changed in the Go fork (daemoncore)

`/library`, `/devices`, `/player/transfer`, `/player/queue`,
`/player/remove_from_queue` were defined as `ApiRequestType` constants with
backing logic in `controls.go`/`player.go` already (used by `mobilebind` for
the Android app), but had **no HTTP route** wired in `api_server.go`, and
`/library` had **no dispatcher case at all** in `handleApiRequest`. Both gaps
are now fixed:

- `daemoncore/api_server.go`: added the 5 missing `http.HandleFunc` routes.
- `daemoncore/player.go`: added the missing `ApiRequestTypeLibrary` case
  (mirrors `mobilebind.ApiGetRootLibraryItems`'s shape: uri/name/description/
  image_url/kind).

**You must `go build ./...` and re-test** — I edited source but couldn't
run your build/test suite (no execute access to your machine, only file
read/write). Sanity-check at minimum:

```
curl http://localhost:24879/library
curl http://localhost:24879/devices
curl -X POST http://localhost:24879/player/transfer -d '{"device_id":"...","play":true}'
```

## HA integration

`homeassistant/custom_components/go_librespot_embedded/` — copy into your HA
`custom_components/` dir, restart HA, add integration, give it host+port.

Files:
- `api.py` — REST client + persistent `/events` WS listener (auto-reconnect)
- `media_player.py` — the entity: playback, volume, shuffle/repeat, browse
  (via `/library`), search (via `/search`), source select = device transfer
  (via `/devices` + `/player/transfer`)
- `config_flow.py`, `__init__.py`, `const.py`, `manifest.json` — boilerplate

## Known gaps / things to verify before treating this as done

1. **`async_search_media` signature is unverified against your installed HA
   core version.** `SearchMedia`/`SearchMediaQuery` types changed across HA
   releases — check `homeassistant/components/media_player/__init__.py` in
   your venv and adjust the import/return shape if it doesn't match.
2. **Browse is flat, one level.** The daemon has no per-playlist/album
   track-listing endpoint (no `/playlist/{uri}/tracks`), so `browse_media`
   only lists rootlist items and plays the whole context on selection —
   same UX as the stock Spotify integration's non-expandable nodes. If you
   want track-level drill-down, that's a separate daemon endpoint to add
   (probably wrapping `sess.Spclient()` playlist-contents calls already
   used elsewhere in `tracks/`).
3. **Volume rounding**: `/player/volume` takes steps out of `volume_steps`,
   not 0-100 - `async_set_volume_level` rounds HA's 0.0-1.0 into that range;
   fine at default 100 steps, lossy at low `volume_steps` configs.
4. Not yet tested end-to-end against a running daemon+HA - only sanity-read
   against your source.
