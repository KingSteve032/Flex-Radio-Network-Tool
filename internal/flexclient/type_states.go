package flexclient

import (
	"net"
	"sync"
	"time"
)

// Route represents one discovered Flextool server route from NetBird.
// - ID: the NetBird route ID string (often includes human label)
// - IP: the /32 address of the route endpoint
type Route struct {
	ID string
	IP net.IP
}

// routeStatus tracks "liveness" for each route.
// We update these timestamps when we receive packets.
type routeStatus struct {
	lastHeartbeat time.Time // last HEARTBEAT packet received
	lastDiscovery time.Time // last discovery packet received (non-heartbeat)
	radioStats    map[string]*radioRXStatus
}

type radioRXStatus struct {
	lastSeen   time.Time
	packetSeen uint64
}

// Shared state:
// - routes: list of Flextool routes we discovered via NetBird
// - routeStatusMap: per-route heartbeat/discovery times
//
// NOTE: This package is designed so the UI can poll Routes() and GetRouteStatus()
// without directly touching internal state.
var (
	routesMu sync.RWMutex
	routes   []Route

	statusMu       sync.RWMutex
	routeStatusMap = make(map[string]*routeStatus)
)
