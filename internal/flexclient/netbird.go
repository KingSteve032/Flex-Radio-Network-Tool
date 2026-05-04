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

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/procutil"
)

// netbirdCLIPath returns the CLI path/name to use.
// If NETBIRD_CLI is set, we use that (useful for testing or portable installs).
func netbirdCLIPath() string {
	if p := os.Getenv("NETBIRD_CLI"); p != "" {
		return p
	}
	return netbirdDefaultCLI
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
