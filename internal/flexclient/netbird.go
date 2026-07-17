package flexclient

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/procutil"
)

const (
	VPNModeNetBird = "netbird"
	VPNModeManual  = "manual"
)

type ManualRoute struct {
	ID string
	IP net.IP
}

var vpnSettings = struct {
	sync.RWMutex
	mode         string
	manualRoutes []ManualRoute
}{
	mode: VPNModeNetBird,
}

func NormalizeVPNMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", VPNModeNetBird:
		return VPNModeNetBird
	case VPNModeManual:
		return VPNModeManual
	default:
		return VPNModeNetBird
	}
}

func SetVPNModeSettings(mode string, manualRoutes []ManualRoute) {
	vpnSettings.Lock()
	defer vpnSettings.Unlock()

	vpnSettings.mode = NormalizeVPNMode(mode)
	vpnSettings.manualRoutes = cloneManualRoutes(manualRoutes)
}

func GetVPNModeSettings() (string, []ManualRoute) {
	vpnSettings.RLock()
	defer vpnSettings.RUnlock()

	return vpnSettings.mode, cloneManualRoutes(vpnSettings.manualRoutes)
}

func cloneManualRoutes(in []ManualRoute) []ManualRoute {
	if len(in) == 0 {
		return nil
	}
	out := make([]ManualRoute, 0, len(in))
	for _, r := range in {
		id := strings.TrimSpace(r.ID)
		if id == "" || r.IP == nil {
			continue
		}
		out = append(out, ManualRoute{
			ID: id,
			IP: append(net.IP(nil), r.IP...),
		})
	}
	return out
}

// discoverFlextoolRoutes runs:
//
//	netbird routes list
//
// Then parses its human-readable output to find route entries where the ID contains "flextool".
// Those routes are treated as "servers" we should connect to.
//
// Output shape from NetBird is not JSON here, so we parse text:
//
//	ID: KC4CAW Flextool
//	Network: 10.10.2.5/32
//
// We store:
// // Route{ID:"KC4CAW Flextool", IP:10.10.2.5}
func discoverFlextoolRoutes() ([]Route, error) {
	mode, manualRoutes := GetVPNModeSettings()
	if len(manualRoutes) > 0 || mode == VPNModeManual {
		routes := routesFromManualRoutes(manualRoutes)
		if len(routes) == 0 {
			return nil, fmt.Errorf("manual mode requires at least one FRNT server route")
		}
		log.Printf("flexclient: using %d manual FRNT server route(s)", len(routes))
		return routes, nil
	}

	if routes, ok, err := discoverManualFlextoolRoutes(); ok || err != nil {
		return routes, err
	}

	cmdPath := netbirdCLIPath()
	log.Printf("flexclient: running NetBird CLI: %s routes list", cmdPath)

	cmd := exec.Command(cmdPath, "routes", "list")
	// HideWindow prevents a console window flashing when the GUI runs commands.
	procutil.HideWindow(cmd)

	out, err := cmd.Output()
	if err != nil {
		log.Printf("flexclient: error running 'netbird routes list': %v", err)
		return nil, fmt.Errorf("failed to run 'netbird routes list': %w", err)
	}

	var outRoutes []Route
	scanner := bufio.NewScanner(bytes.NewReader(out))

	var currentID string
	var isFlextool bool

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Detect route ID line
		if strings.Contains(line, "ID:") {
			idx := strings.Index(line, "ID:")
			if idx >= 0 {
				currentID = strings.TrimSpace(line[idx+len("ID:"):])
				isFlextool = strings.Contains(strings.ToLower(currentID), "flextool")
			}
			continue
		}

		// Detect network line for a Flextool route
		if isFlextool && strings.Contains(line, "Network:") {
			idx := strings.Index(line, "Network:")
			if idx < 0 {
				continue
			}

			netStr := strings.TrimSpace(line[idx+len("Network:"):])
			parts := strings.SplitN(netStr, "/", 2)
			if len(parts) == 0 {
				continue
			}

			ipStr := strings.TrimSpace(parts[0])
			ip := net.ParseIP(ipStr)
			if ip == nil {
				log.Printf("flexclient: failed to parse IP from NetBird route: %q", ipStr)
				continue
			}

			outRoutes = append(outRoutes, Route{
				ID: currentID,
				IP: ip,
			})

			log.Printf("flexclient: discovered Flextool route: ID=%q IP=%s", currentID, ip.String())

			// Reset state: only one Network per ID block
			isFlextool = false
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("flexclient: scanner error parsing 'netbird routes list': %v", err)
		return nil, fmt.Errorf("scanner error parsing 'netbird routes list': %w", err)
	}

	log.Printf("flexclient: total Flextool routes discovered: %d", len(outRoutes))
	return outRoutes, nil
}

func routesFromManualRoutes(manualRoutes []ManualRoute) []Route {
	out := make([]Route, 0, len(manualRoutes))
	for _, r := range manualRoutes {
		id := strings.TrimSpace(r.ID)
		if id == "" || r.IP == nil {
			continue
		}
		out = append(out, Route{ID: id, IP: append(net.IP(nil), r.IP...)})
		log.Printf("flexclient: discovered manual Flextool route: ID=%q IP=%s", id, r.IP.String())
	}
	return out
}

func discoverManualFlextoolRoutes() ([]Route, bool, error) {
	raw := strings.TrimSpace(os.Getenv("FLEXCLIENT_ROUTES"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("FRNT_SERVER_ROUTES"))
	}
	if raw == "" {
		return nil, false, nil
	}

	manualRoutes, err := ParseManualRoutesText(raw)
	if err != nil {
		return nil, true, err
	}
	out := routesFromManualRoutes(manualRoutes)
	if len(out) == 0 {
		return nil, true, fmt.Errorf("FLEXCLIENT_ROUTES was set but no valid routes were provided")
	}
	log.Printf("flexclient: total manual Flextool routes discovered: %d", len(out))
	return out, true, nil
}

func ParseManualRoutesText(raw string) ([]ManualRoute, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	tokens := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]ManualRoute, 0, len(tokens))
	for i, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		id := fmt.Sprintf("FRNT Manual Route %d", i+1)
		ipText := token
		if strings.Contains(token, "=") {
			parts := strings.SplitN(token, "=", 2)
			id = strings.TrimSpace(parts[0])
			ipText = strings.TrimSpace(parts[1])
		}
		if id == "" {
			id = fmt.Sprintf("FRNT Manual Route %d", i+1)
		}
		ip := net.ParseIP(strings.TrimSpace(ipText))
		if ip == nil {
			return nil, fmt.Errorf("invalid manual FRNT server route %q", token)
		}
		out = append(out, ManualRoute{ID: id, IP: ip})
	}
	return out, nil
}

func FormatManualRoutesText(routes []ManualRoute) string {
	if len(routes) == 0 {
		return ""
	}
	lines := make([]string, 0, len(routes))
	for _, route := range routes {
		if strings.TrimSpace(route.ID) == "" || route.IP == nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%s", strings.TrimSpace(route.ID), route.IP.String()))
	}
	return strings.Join(lines, "\n")
}
