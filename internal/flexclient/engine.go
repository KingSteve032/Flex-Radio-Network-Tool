package flexclient

import (
	"context"
	"fmt"
	"log"
)

// Start runs the flexclient engine until ctx is cancelled.
//
// The flow is:
//  1. Discover Flextool routes from NetBird
//  2. Initialize per-route status
//  3. Spawn one worker goroutine per route (runForServer)
//  4. Wait until ctx is cancelled
//  5. Clear shared state so UI reflects "stopped"
//
// startupResult receives exactly one value:
// - nil: startup succeeded and workers are running
// - error: startup failed before workers could run
// The channel is closed after sending this value.
func Start(ctx context.Context, version string, startupResult chan<- error) {
	notifyStartup := func(err error) {
		if startupResult == nil {
			return
		}
		startupResult <- err
		close(startupResult)
	}

	log.Printf("flexclient: start (version=%s)", version)

	routesFound, err := discoverFlextoolRoutes()
	if err != nil {
		log.Printf("flexclient: discoverFlextoolRoutes error: %v", err)
		clearState()
		notifyStartup(fmt.Errorf("route discovery failed: %w", err))
		return
	}
	if len(routesFound) == 0 {
		log.Printf("flexclient: no Flextool routes found, nothing to do")
		clearState()
		notifyStartup(fmt.Errorf("no Flextool routes found"))
		return
	}

	// Reset route status tracking
	initRouteStatusFor(routesFound)

	// Publish routes for UI consumption
	routesMu.Lock()
	routes = routesFound
	routesMu.Unlock()

	// Spin up a worker per server route
	for _, r := range routesFound {
		route := r
		log.Printf("flexclient: launching server handler for route %s (%s)", route.ID, route.IP.String())
		go runForServer(ctx, route, version)
	}

	notifyStartup(nil)

	// Block until Stop()
	<-ctx.Done()
	log.Printf("flexclient: context cancelled, clearing state")

	// Clear routes + statuses so the UI list empties cleanly
	clearState()
}

func clearState() {
	routesMu.Lock()
	routes = nil
	routesMu.Unlock()

	statusMu.Lock()
	routeStatusMap = make(map[string]*routeStatus)
	statusMu.Unlock()

	resetSerialOwnership()
	resetIdentityCache()
}

// Routes returns a snapshot copy of the currently known Flextool routes.
// The UI polls this to populate the list.
func Routes() []Route {
	routesMu.RLock()
	defer routesMu.RUnlock()

	out := make([]Route, len(routes))
	copy(out, routes)
	return out
}
