package flexclient

import "testing"

func TestDiscoverManualFlextoolRoutes(t *testing.T) {
	t.Setenv("FLEXCLIENT_ROUTES", "Chesapeake=100.64.0.4,100.64.0.5")
	t.Setenv("FRNT_SERVER_ROUTES", "")

	routes, ok, err := discoverManualFlextoolRoutes()
	if err != nil {
		t.Fatalf("discoverManualFlextoolRoutes returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected manual routes to be enabled")
	}
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	if routes[0].ID != "Chesapeake" || routes[0].IP.String() != "100.64.0.4" {
		t.Fatalf("unexpected first route: %+v", routes[0])
	}
	if routes[1].ID != "FRNT Manual Route 2" || routes[1].IP.String() != "100.64.0.5" {
		t.Fatalf("unexpected second route: %+v", routes[1])
	}
}

func TestDiscoverManualFlextoolRoutesInvalidIP(t *testing.T) {
	t.Setenv("FLEXCLIENT_ROUTES", "bad-route")
	t.Setenv("FRNT_SERVER_ROUTES", "")

	_, ok, err := discoverManualFlextoolRoutes()
	if !ok {
		t.Fatal("expected manual routes to be enabled")
	}
	if err == nil {
		t.Fatal("expected invalid manual route to return an error")
	}
}
