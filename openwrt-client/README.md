# FRNT OpenWrt Client for GL.iNet GL-AXT1800

This is an **FRNT client** for the router, not an FRNT server.

The GL-AXT1800 joins NetBird as a VPN peer, registers with your existing
FRNT server on **UDP 14992**, receives validated FlexRadio discovery
frames, and rebroadcasts those frames only on the chosen local LAN
(for example `br-lan` at `192.168.8.1/24`) via **UDP 4992**.

This is a **discovery-only** release. SmartSDR control, VITA stream
routing and remote radio connections require additional VPN routing and
possibly proxy/NAT features. Discovery alone does not provide radio access.

## Download (GitHub Actions, no Go required on the router)

Public release: https://github.com/KingSteve032/Flex-Radio-Network-Tool/releases/tag/openwrt-client-v0.1.2

Two packages are built and independently validated by GitHub Actions:

1. `frnt-openwrt-client_0.1.2_aarch64_cortex-a53_neon-vfpv4.ipk`:
   ARM64 headless client and OpenWrt `procd` service.
2. `luci-app-frnt-client_0.1.2_all.ipk`:
   Optional web interface (LuCI > Services > FRNT Discovery Client).
   Requires LuCI already installed on the GL.iNet router.
   Its dependency on `luci-base` and `rpcd-mod-file` must be satisfied.

Also download `SHA256SUMS.txt` to verify the package files.

**The previous v0.1.1 installer was packaged incorrectly as an `ar`
archive. Version 0.1.2 uses the OpenWrt-standard gzip-compressed tar
IPK container.** The original failure was not caused by ARM64 support.

Target: GL.iNet GL-AXT1800 / OpenWrt 23.05-SNAPSHOT /
`aarch64_cortex-a53_neon-vfpv4`. Confirm the router's architecture using
`opkg print-architecture` before installing. This version is tested by
GitHub Actions, **not yet tested on physical GL-AXT1800 hardware**.

## Router installation

The person with the router should download the two IPKs from the release
and copy both to the router's `/tmp` folder (e.g. SCP). On the router:

```sh
opkg print-architecture
opkg install /tmp/frnt-openwrt-client_0.1.2_aarch64_cortex-a53_neon-vfpv4.ipk
```

If LuCI is installed, install the separate frontend as well:

```sh
opkg install /tmp/luci-app-frnt-client_0.1.2_all.ipk
/etc/init.d/rpcd restart
/etc/init.d/uhttpd restart
```

If LuCI or its dependencies are missing, first install LuCI using the
GL.iNet admin interface at **System > Advanced Settings**. If the vendor
firmware package feeds cannot provide the required LuCI dependencies,
leave the UI package uninstalled; the CLI service still works.

The router's GL.iNet admin UI is not modified. Launch its LuCI
**Advanced Settings** page, then select **Services > FRNT Discovery
Client**. The page provides configuration fields, Start/Stop/Restart,
status, NetBird interface details, and recent FRNT logs. No public
unauthenticated control endpoint is opened.

## Configure

Ensure the NetBird peer is already connected and can reach the FRNT
server VPN IP. For example, in the diagnostic report below,
the router's NetBird IP is `100.85.12.187` on `wt0`,
while the example server VPN IP is `100.85.50.97`.

Set the **actual** FRNT server's NetBird IP and interfaces:

```sh
uci set frnt-client.main.server='100.85.50.97'
uci set frnt-client.main.netbird_routes='0'
uci set frnt-client.main.vpn_interface='wt0'
uci set frnt-client.main.lan_interface='br-lan'
uci commit frnt-client
/etc/init.d/frnt-client start
/etc/init.d/frnt-client status
logread -e FRNT
```

To use the UI, edit the same values through LuCI and click **Save & Apply**
before pressing **Restart**.

NetBird's `wt0` interface is mandatory for the FRNT control channel.
FRNT uses `SO_BINDTODEVICE` and verifies its source address belongs to
`wt0`, preventing fallback to the WAN. A missing or not-yet-up
NetBird interface causes retries every 10 seconds. The client does not
manage NetBird, its keys, or its routing.

To stop or restart:

```sh
/etc/init.d/frnt-client stop
/etc/init.d/frnt-client restart
```

Enable/disable startup on boot after confirming it works:

```sh
/etc/init.d/frnt-client enable
/etc/init.d/frnt-client disable
```

### Optional NetBird route discovery

Manual VPN server IP is recommended; it works on older OpenWrt builds.

If your NetBird CLI supports `netbird routes list` and there is exactly
one selected `Flextool` `/32` host route, you can use it instead:

```sh
netbird routes list
uci delete frnt-client.main.server
uci set frnt-client.main.netbird_routes='1'
uci commit frnt-client
/etc/init.d/frnt-client restart
```

If this fails, return to `netbird_routes=0` and configure the server IP
explicitly. NetBird release versions do not all expose identical CLI
commands or route formats.

## Troubleshooting "connection refused"

Example router logs:

```
FRNT client started: server=100.85.50.97:14992 VPN=wt0 LAN=br-lan ...
read udp4 100.85.12.187:35989->100.85.50.97:14992: read: connection refused
```

This shows the client selected the correct NetBird-side IP address and
`wt0` is working locally. The destination (or a firewall in between)
is rejecting UDP 14992, commonly because there is no FRNT process
listening on that IP and port. This is **not** a broadcast failure.

On the FRNT **server machine** at the NetBird IP:

```sh
ip -4 addr show
sudo ss -lunp | grep ':14992'
sudo systemctl status frnt-listen --no-pager
sudo journalctl -u frnt-listen -n 80 --no-pager
```

The upstream FRNT server startup requires:

- `--mode server listen` (not a client GUI process);
- `SEND_INTERFACE` set to the server's NetBird interface name;
- the default UDP port 14992, or `--port 14992`;
- a valid `LISTEN_INTERFACE` for capturing FlexRadio LAN discovery;
- NetBird ACLs and the host firewall permitting the router to reach
  server UDP 14992.

If the listener exists but discovery is absent, check the server
`CLIENT_AUTH_MODE` and client authorization (either registered mode
or the server's NetBird database). Authorization rejection generally
wouldn't yield a UDP port-unreachable error; it would silently withhold
registration and heartbeats.

On the router:

```sh
ip -4 addr show dev wt0
ip route get 100.85.50.97
netbird status
logread -e FRNT
```

Do not open the FRNT UDP control port to the public Internet.

## Development

Source is under `openwrt-client/`. GitHub Actions compiles a static
Linux ARM64 Go executable with Go 1.25, packages two OpenWrt-compatible
IPKs using outer tar+gzip format, verifies package contents and produces
SHA256 checksums. No Go toolchain is required on the router.

The frontend is a LuCI JavaScript view with explicit rpcd ACLs.
It only permits UCI configuration for `frnt-client` and narrowly
whitelisted service/query helper programs. It does not grant shell
execution to the web UI.

CI tests are not a substitute for validation on the GL-AXT1800, including
its vendor LuCI and opkg versions. No deployment has been attempted here.
