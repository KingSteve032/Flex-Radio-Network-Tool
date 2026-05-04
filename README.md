# Flex-Radio-Network-Tool
A tool built for the W4CAR Flex Radio Network.

This project now includes both:
- `client` mode: Windows GUI app that discovers/rebroadcasts FlexRadio packets locally
- `server` mode: NetBird-side relay tooling (`listen`, `sync`, `info`, `pcap`, `version`)

## Run Modes

Client mode (default):

```powershell
go run . --mode client
```

Server mode:

```powershell
go run . --mode server listen
go run . --mode server sync -d
go run . --mode server info -l
```

To see server command help:

```powershell
go run . --mode server --help
```

### Server Auto-Sync

In `server listen` mode, you can enable built-in periodic NetBird API sync
without cron by setting:

```env
SYNC_INTERVAL_SECONDS=60
```

- `0` disables periodic sync (manual `server sync` only).
- Any value `> 0` runs an initial sync at startup and repeats at that interval.
- Requires `NETBIRD_API_TOKEN` and `NETBIRD_API_URL` in `.flextool`.

### Per-Radio Proxy vs Direct (No Auto-Swap)

The client now supports explicit per-radio mode selection:

- `direct`: keep existing behavior (discovery rebroadcast as-is)
- `proxy`: rewrite that radio's discovery endpoint so SmartSDR connects through the Flextool server proxy path

There is no automatic fallback/swap between modes.

#### Client Setting

Set `FLEXCLIENT_RADIO_MODES` on the client:

```env
FLEXCLIENT_RADIO_MODES=SERIAL1=proxy,SERIAL2=direct
```

- Any radio not listed defaults to `direct`.
- Mode keys are compared case-insensitively.

Optional client setting:

```env
FLEXCLIENT_PROXY_BASE_PORT=30000
```

This base is used to derive deterministic per-radio proxy TCP ports from serials.

#### Server Settings

In server `.flextool`:

```env
ENABLE_VITA_PROXY=true
VITA_PROXY_PORT=4991
PROXY_BASE_PORT=30000
MULTI_PROXY=true
```

- `ENABLE_VITA_PROXY=true` enables the VITA relay path.
- `PROXY_BASE_PORT` must match client `FLEXCLIENT_PROXY_BASE_PORT` to keep per-radio proxy port mapping aligned.
- `MULTI_PROXY=true` allows one client to keep simultaneous proxy sessions to multiple radios.

## Platform Notes

- Linux defaults to `server` mode when `--mode` is omitted.
- Windows and macOS default to `client` mode when `--mode` is omitted.
- `server` mode uses `libpcap` (required for `listen`/`pcap`) and SQLite (`sync` db storage).
