package flexclient

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/procutil"
)

type RouteRadioStatus struct {
	Serial     string
	LastSeen   time.Time
	PacketSeen uint64
}

// CheckNetbirdStatus runs `netbird status` and returns:
//
// - connected: true if "Management: Connected" is found
// - needsLogin: true if "Daemon status: NeedsLogin" is found
// - raw: the raw CLI output (useful for logs / debugging UI)
// - err: command execution error (netbird missing, etc.)
//
// This lets the GUI block "Start" until NetBird is connected.
func CheckNetbirdStatus(timeout time.Duration) (connected bool, needsLogin bool, raw string, err error) {
	mode, manualRoutes := GetVPNModeSettings()
	if mode == VPNModeManual || len(manualRoutes) > 0 {
		log.Printf("flexclient: VPN status check skipped for manual FRNT server routes")
		return true, false, "VPN status check skipped for manual FRNT server routes", nil
	}

	if shouldSkipVPNStatusCheck() {
		log.Printf("flexclient: VPN status check skipped by configuration")
		return true, false, "VPN status check skipped", nil
	}

	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmdPath := netbirdCLIPath()
	cmd := exec.CommandContext(ctx, cmdPath, "status")
	procutil.HideWindow(cmd)

	out, err := cmd.CombinedOutput()
	raw = string(out)

	if ctx.Err() == context.DeadlineExceeded {
		log.Printf("flexclient: netbird status timed out after %s", timeout)
		return false, false, raw, fmt.Errorf("netbird status timed out after %s", timeout)
	}

	if err != nil {
		log.Printf("flexclient: netbird status error: %v, output:\n%s", err, raw)
		return false, false, raw, err
	}

	// Explicit login required case:
	if strings.Contains(raw, "Daemon status: NeedsLogin") {
		log.Printf("flexclient: netbird status -> NeedsLogin")
		return false, true, raw, nil
	}

	// Connected to management plane:
	if strings.Contains(raw, "Management: Connected") {
		log.Printf("flexclient: netbird status -> Management: Connected")
		return true, false, raw, nil
	}

	// Any other situation is treated as not connected.
	log.Printf("flexclient: netbird status -> not connected, output:\n%s", raw)
	return false, false, raw, nil
}

func shouldSkipVPNStatusCheck() bool {
	for _, name := range []string{"FLEXCLIENT_SKIP_VPN_STATUS", "FRNT_SKIP_VPN_STATUS"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
		case "1", "true", "yes", "y", "on":
			return true
		}
	}
	return false
}

// GetRouteStatus returns how long ago we saw:
// - heartbeatAgo: "HEARTBEAT..." packets (server keepalive)
// - discoveryAgo: discovery packets (SmartSDR discovery frames)
//
// hasHB/hasRX indicate if we've ever seen those packets.
func GetRouteStatus(routeID string) (heartbeatAgo, discoveryAgo time.Duration, hasHB, hasRX bool) {
	statusMu.RLock()
	defer statusMu.RUnlock()

	s, ok := routeStatusMap[routeID]
	if !ok {
		return 0, 0, false, false
	}

	if !s.lastHeartbeat.IsZero() {
		heartbeatAgo = time.Since(s.lastHeartbeat)
		hasHB = true
	}
	if !s.lastDiscovery.IsZero() {
		discoveryAgo = time.Since(s.lastDiscovery)
		hasRX = true
	}
	return
}

// initRouteStatusFor resets the status map for a new set of routes.
// Called when Start() discovers routes.
func initRouteStatusFor(rs []Route) {
	statusMu.Lock()
	defer statusMu.Unlock()

	routeStatusMap = make(map[string]*routeStatus)
	for _, r := range rs {
		routeStatusMap[r.ID] = &routeStatus{
			radioStats: make(map[string]*radioRXStatus),
		}
	}
	log.Printf("flexclient: initialized route status map for %d routes", len(rs))
}

// markHeartbeat updates the "last heartbeat" timestamp for a route.
// Called when we receive a HEARTBEAT packet from the server.
func markHeartbeat(routeID string) {
	statusMu.Lock()
	defer statusMu.Unlock()

	s, ok := routeStatusMap[routeID]
	if !ok {
		s = &routeStatus{
			radioStats: make(map[string]*radioRXStatus),
		}
		routeStatusMap[routeID] = s
	}
	s.lastHeartbeat = time.Now()
}

// markDiscovery updates the "last discovery data" timestamp for a route.
// Called when we receive a discovery packet and rebroadcast it.
func markDiscovery(routeID, radioSerial string) {
	statusMu.Lock()
	defer statusMu.Unlock()

	s, ok := routeStatusMap[routeID]
	if !ok {
		s = &routeStatus{
			radioStats: make(map[string]*radioRXStatus),
		}
		routeStatusMap[routeID] = s
	}
	now := time.Now()
	s.lastDiscovery = now

	radioSerial = strings.ToLower(strings.TrimSpace(radioSerial))
	if radioSerial == "" {
		return
	}

	if s.radioStats == nil {
		s.radioStats = make(map[string]*radioRXStatus)
	}

	rs, ok := s.radioStats[radioSerial]
	if !ok {
		rs = &radioRXStatus{}
		s.radioStats[radioSerial] = rs
	}

	rs.lastSeen = now
	rs.packetSeen++
}

// GetRouteRadioStatuses returns radios seen on this route in deterministic
// serial order so UI rows stay stable while packet counters refresh.
func GetRouteRadioStatuses(routeID string) []RouteRadioStatus {
	statusMu.RLock()
	defer statusMu.RUnlock()

	s, ok := routeStatusMap[routeID]
	if !ok || len(s.radioStats) == 0 {
		return nil
	}

	out := make([]RouteRadioStatus, 0, len(s.radioStats))
	for serial, st := range s.radioStats {
		out = append(out, RouteRadioStatus{
			Serial:     serial,
			LastSeen:   st.lastSeen,
			PacketSeen: st.packetSeen,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Serial < out[j].Serial
	})

	return out
}
