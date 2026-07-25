package flexclient

import (
	"net"
	"testing"
	"time"
)

func TestSerialOwnerPrefersMatchingRouteSubnetAndRemovesStaleRoute(t *testing.T) {
	resetSerialOwnership()
	t.Cleanup(resetSerialOwnership)

	initRouteStatusFor([]Route{
		{ID: "Site 1", IP: mustParseTestIP("10.1.0.4")},
		{ID: "Site 2", IP: mustParseTestIP("10.2.0.4")},
	})

	now := time.Now()
	serial := "1121-1104-6700-2912"

	if !claimSerialOwnerWithScore("Site 1", serial, now, routeRadioAffinityScore(mustParseTestIP("10.1.0.4"), "10.2.0.12")) {
		t.Fatal("expected first route to claim owner")
	}
	markDiscovery("Site 1", serial)
	if got := GetRouteRadioStatuses("Site 1"); len(got) != 1 {
		t.Fatalf("expected radio initially under Site 1, got %d", len(got))
	}

	if !claimSerialOwnerWithScore("Site 2", serial, now.Add(time.Second), routeRadioAffinityScore(mustParseTestIP("10.2.0.4"), "10.2.0.12")) {
		t.Fatal("expected matching route subnet to take ownership")
	}
	markDiscovery("Site 2", serial)

	if got := GetRouteRadioStatuses("Site 1"); len(got) != 0 {
		t.Fatalf("expected radio removed from stale Site 1, got %+v", got)
	}
	if got := GetRouteRadioStatuses("Site 2"); len(got) != 1 || got[0].Serial != serial {
		t.Fatalf("expected radio under Site 2, got %+v", got)
	}
}

func mustParseTestIP(raw string) net.IP {
	ip := net.ParseIP(raw)
	if ip == nil {
		panic("invalid test IP: " + raw)
	}
	return ip
}
