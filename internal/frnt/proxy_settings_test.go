package frnt

import (
	"strings"
	"testing"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexclient"
)

func TestNormalizeVPNModeText(t *testing.T) {
	if got := normalizeVPNModeText("manual"); got != "manual" {
		t.Fatalf("expected manual, got %q", got)
	}
	if got := normalizeVPNModeText("not-a-mode"); got != "netbird" {
		t.Fatalf("expected unknown mode to fall back to netbird, got %q", got)
	}
	if got := normalizeVPNModeText(""); got != "netbird" {
		t.Fatalf("expected empty mode to default to netbird, got %q", got)
	}
}

func TestRadioModeTextUsesOnOff(t *testing.T) {
	modes, err := parseRadioModesText("abc=on\ndef=off\nlegacy=proxy")
	if err != nil {
		t.Fatalf("parse radio modes: %v", err)
	}
	if modes["abc"] != "direct" {
		t.Fatalf("expected on to normalize to direct, got %q", modes["abc"])
	}
	if modes["def"] != "off" {
		t.Fatalf("expected off, got %q", modes["def"])
	}
	if modes["legacy"] != "direct" {
		t.Fatalf("expected legacy proxy to normalize to direct, got %q", modes["legacy"])
	}

	formatted := formatRadioModesText(modes)
	if strings.Contains(formatted, "proxy") || strings.Contains(formatted, "direct") {
		t.Fatalf("formatted modes should only use on/off, got %q", formatted)
	}
}

func TestManualRouteSettingsRoundTrip(t *testing.T) {
	routes, err := flexclient.ParseManualRoutesText("Chesapeake=100.64.0.2\nW4CAR=100.64.0.5")
	if err != nil {
		t.Fatalf("parse manual routes: %v", err)
	}

	files := manualRoutesToSettings(routes)
	if len(files) != 2 {
		t.Fatalf("expected 2 route files, got %d", len(files))
	}

	roundTrip := manualRoutesFromSettings(files)
	if len(roundTrip) != 2 {
		t.Fatalf("expected 2 round-trip routes, got %d", len(roundTrip))
	}
	if roundTrip[0].ID != "Chesapeake" || roundTrip[0].IP.String() != "100.64.0.2" {
		t.Fatalf("unexpected first route: %+v", roundTrip[0])
	}
}
