# FRNT OpenWrt Discovery Client (GL-AXT1800)

A **router-side client**, not the FRNT server. It connects over the router's
existing VPN route to an FRNT server on UDP 14992, sends periodic FRNT `HELLO`
registrations, validates incoming modern FlexRadio VRT discovery frames, and
rebroadcasts them from **one explicitly chosen LAN interface** to UDP 4992.

This is a headless, discovery-only OpenWrt implementation of the upstream
FRNT client's HELLO/discovery protocol. It deliberately does **not** use the
desktop GUI, run libpcap/SQLite, expose a listener, or broadcast onto the WAN.
It is **not** a full replacement for the upstream Flexclient's SmartSDR
TCP/direct-assist and VITA stream handling.

Upstream protocol source: https://github.com/KingSteve032/Flex-Radio-Network-Tool
Reviewed against: upstream `main` as of 2026-11-06, release v0.2.15.

## Target

- GL.iNet GL-AXT1800 (Slate AX), OpenWrt 23.05-SNAPSHOT
- `aarch64` kernel and `aarch64_cortex-a53_neon-vfpv4` opkg architecture
- NetBird installed, connected, and managing the `wt0` VPN interface
- FRNT server VPN IPv4 reachable across NetBird (no WAN fallback)
- LAN bridge typically `br-lan` (verify on the actual router)
- The router and FRNT server must be allowed to exchange UDP 14992 by NetBird ACL policy

## Downloadable GitHub Actions build (recommended)

The `Build FRNT OpenWrt Client` GitHub Actions workflow runs tests, builds an
ARM64 `.ipk`, checks its package layout, generates `SHA256SUMS.txt`, and
uploads the files to the **public GitHub Releases page**.

Download the package at:

https://github.com/KingSteve032/Flex-Radio-Network-Tool/releases/tag/openwrt-client-v0.1.1

The person installing on the router **does not need Go, DevSpace, or a GitHub
account** to download a public release asset.

If the workflow has not finished successfully, that release will not be
available yet. Check the repository Actions tab for the build result.

## Build locally on a Go-equipped host

Requires Go 1.25+, GNU `ar`, and `tar`. Building uses a statically linked
Linux/ARM64 Go executable, so no on-router Go SDK or libpcap is required.

```sh
cd openwrt-client
go test -mod=mod ./...
bash build-ipk.sh
```

The build outputs:
`dist/frnt-openwrt-client_0.1.1_aarch64_cortex-a53_neon-vfpv4.ipk`

**Not yet built or tested on the GL-AXT1800.** Verify the router's opkg
architecture and firmware version before installing. A vendor-specific
OpenWrt snapshot may need additional checks.

## Configure and start on the router

Copy the IPK to the router, then:

```sh
opkg install /tmp/frnt-openwrt-client_0.1.1_aarch64_cortex-a53_neon-vfpv4.ipk
netbird status -d
ip -4 addr show dev wt0
uci set frnt-client.main.server='YOUR_FRNT_SERVER_VPN_IPV4'
uci set frnt-client.main.vpn_interface='wt0'
uci set frnt-client.main.lan_interface='br-lan'
uci commit frnt-client
/etc/init.d/frnt-client start
/etc/init.d/frnt-client status
logread -e frnt
```

Stop or restart:

```sh
/etc/init.d/frnt-client stop
/etc/init.d/frnt-client restart
```

Enable automatic startup only after a successful manual test:

```sh
/etc/init.d/frnt-client enable
```

Disable automatic startup (does not stop an already-running process):

```sh
/etc/init.d/frnt-client disable
```

## NetBird integration

NetBird owns the VPN. FRNT does not start, stop, configure, or log in to NetBird.
The FRNT UDP socket is **bound to wt0** using Linux `SO_BINDTODEVICE`
and its chosen source IP is validated against that interface, so it will fail
closed rather than trying the WAN when the VPN is absent.

The **recommended configuration** is to set the FRNT server's NetBird VPN IP
explicitly using the UCI `server` setting above. This works with older
OpenWrt-packaged NetBird versions, including those without route discovery.

If your installed NetBird CLI supports `netbird routes list` and you already
advertise a route **whose ID contains Flextool**, you may instead enable
upstream-style automatic discovery of a single selected IPv4 /32 route:

```sh
netbird routes list
uci delete frnt-client.main.server
uci set frnt-client.main.netbird_routes='1'
uci commit frnt-client
/etc/init.d/frnt-client restart
```

This is optional. If the command is unavailable, the route is not a /32 host,
or multiple Flextool routes are advertised, return to an explicit server IP:

```sh
uci set frnt-client.main.netbird_routes='0'
uci set frnt-client.main.server='YOUR_FRNT_SERVER_VPN_IPV4'
uci commit frnt-client
/etc/init.d/frnt-client restart
```

**Important:** On OpenWrt 23.05, NetBird's package may be older than the
version that provides `routes list`. We have not verified which commands your
router's eventual NetBird installation supports.

## Operational behavior

- Transmits `HELLO client_ip=<router VPN-side source IP> client_version=openwrt-0.1.1`.
- Sends HELLO every 30 seconds; counts server HEARTBEAT packets.
- Rebroadcasts only recognized modern VRT discovery payloads, with radio
  serial, IPv4 and port metadata, on the configured LAN subnet.
- Keeps fresh discoveries available for up to 4 seconds via cached rebroadcast;
  discards stale entries at 20 seconds.
- Uses a specific directed-broadcast destination (for example
  `192.168.8.255:4992`), never indiscriminately sends to all interfaces.
- Starts only with a configured server IP or explicit NetBird route discovery.
  Does not autostart by default.
- Fails closed when `wt0` is unavailable, down, or the selected source address
  does not belong to the VPN interface.
- procd manages the service; logs go to OpenWrt's regular ring-buffer logs.

## Important limitations

**Discovery is not radio connectivity.** SmartSDR on a LAN machine may see a
remote radio, but cannot connect unless the advertised radio IP/port is
reachable from that machine. The router must supply appropriate VPN routes,
firewall forwarding and potentially a LAN-reachable TCP/VITA proxy to match
the desktop client's direct-assist functionality. No NAT, routes or
firewall rules are changed by this package. NetBird access to the server
alone does not automatically give LAN computers access to remote radio IPs.

Only use an FRNT server you trust, on a protected network. The upstream
registration protocol uses source/HELLO state, not cryptographic
application-layer authentication. Do not expose UDP 14992 publicly.

The GL.iNet UI/LuCI Start/Stop screen is planned separately; this initial
package offers service start/stop/restart via OpenWrt's standard init tools.
