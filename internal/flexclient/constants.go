package flexclient

import "time"

// This file contains package-wide constants that control networking behavior.
// Keeping them in one place makes tuning easier and avoids "magic numbers"
// scattered across the code.
const (
	// UDP port that each Flextool server listens on (your relay service).
	serverPort = 14992

	// Default NetBird CLI name. Can be overridden by NETBIRD_CLI environment variable.
	netbirdDefaultCLI = "netbird"

	// How often we send HELLO keep-alives to each Flextool server.
	helloInterval = 30 * time.Second

	// FlexRadio discovery broadcast port on the local LAN.
	// We rebroadcast received discovery packets to 255.255.255.255:4992
	// so SmartSDR sees the radios as "local".
	broadcastPort = 4992

	// Optional VITA proxy envelope marker.
	vitaProxyPacketMagic = "VITAP1"

	// How often we rebroadcast cached discovery packets to keep SmartSDR
	// entries alive even if upstream discovery cadence is jittery.
	discoveryRebroadcastInterval = 1 * time.Second

	// Drop cached discoveries after this age so stale radios naturally disappear.
	// Keep this generously high to tolerate upstream discovery jitter.
	discoveryCacheMaxAge = 5 * time.Minute

	// If the same radio serial is seen from multiple routes, keep one route as
	// the owner for this long before allowing takeover. This prevents SmartSDR
	// flapping when duplicate discovery streams arrive from multiple servers.
	serialOwnerHold = 8 * time.Second
)
