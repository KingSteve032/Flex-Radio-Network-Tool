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

## Versioning

- Single source of truth: `internal/buildinfo/buildinfo.go`
- Runtime check:
  - `frnt --version`
  - `frnt --mode server version`

For CI/release builds, version metadata is injected via `-ldflags`:
- `Version` (tag like `v0.2.0`)
- `Commit` (short SHA)
- `BuildDate` (UTC timestamp)
- Local/dev default version comes from `internal/buildinfo/buildinfo.go`.

## App Icon (`icon.png`)

- Runtime window/app icon is loaded from:
  - `icon.png` next to executable, or
  - `assets/icon.png`
- Fyne packaging metadata is in `FyneApp.toml` with:
  - `Icon = "assets/icon.png"`

If you package desktop apps manually with Fyne, the icon metadata is picked up automatically:

```bash
go install fyne.io/tools/cmd/fyne@latest
fyne package -os windows -release
fyne package -os darwin -release
```

## GitHub Releases

Workflow file: `.github/workflows/release.yml`

What it does:
- Builds binaries on native runners for:
  - Linux `amd64`
  - Linux `arm64`
  - Linux `armv7` (Raspberry Pi 3/4 32-bit OS)
  - Windows `amd64`
  - macOS `amd64` and `arm64`
- Uploads artifacts for every run.
- On tag push `v*`, creates a GitHub Release and attaches the binaries.

How to publish:
1. Update `internal/buildinfo/buildinfo.go` (if needed for local default version).
2. Commit to `main`.
3. Tag and push:
   - `git tag v0.2.0`
   - `git push origin v0.2.0`
4. GitHub Actions builds and publishes release assets automatically.

## Server Update From GitHub Release

Use script: `scripts/update-server-from-release.sh`

Examples:

```bash
# latest release
bash scripts/update-server-from-release.sh

# specific tag
bash scripts/update-server-from-release.sh v0.2.0
```

The script auto-detects architecture and installs:
- `frnt-linux-amd64`
- `frnt-linux-arm64`
- `frnt-linux-armv7`

Then it restarts `frnt-listen.service`.

## Client Installers (Stable Install Path)

To keep client updates in the same location every time:

- Windows (installs to `C:\Program Files\Flex Radio Network Tool\frnt.exe`):
  - `powershell -ExecutionPolicy Bypass -File scripts/install-client-windows.ps1`
  - Specific release: `powershell -ExecutionPolicy Bypass -File scripts/install-client-windows.ps1 -Version v0.2.6`

- macOS (installs to `/Applications/Flex Radio Network Tool/frnt`):
  - `bash scripts/install-client-macos.sh`
  - Specific release: `bash scripts/install-client-macos.sh v0.2.6`
