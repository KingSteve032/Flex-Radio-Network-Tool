package main

import (
	"strings"
	"testing"
)

func TestParseNetBirdRoutes(t *testing.T) {
	routes := `Available Routes:
 - ID: Other Route
   Network: 10.1.2.4/32
   Status: Selected
 - ID: KC4CAW Flextool
   Network: 10.2.4.5/32
   Status: Selected
 - ID: Old Flextool
   Network: 10.2.5.6/32
   Status: Deselected
`
	ip, err := parseNetBirdRoutes([]byte(routes))
	if err != nil || ip.String() != "10.2.4.5" {
		t.Fatalf("unexpected route: ip=%v err=%v", ip, err)
	}
}

func TestParseNetBirdRoutesRejectsAmbiguous(t *testing.T) {
	input := `- ID: A Flextool
  Network: 10.2.4.5/32
  Status: Selected
- ID: B Flextool
  Network: 10.2.5.6/32
  Status: Selected
`
	_, err := parseNetBirdRoutes([]byte(input))
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("expected ambiguous route error, got %v", err)
	}
}

func TestParseNetBirdRoutesRejectsSubnets(t *testing.T) {
	_, err := parseNetBirdRoutes([]byte("- ID: A Flextool\n  Network: 10.2.4.0/24\n"))
	if err == nil {
		t.Fatal("subnet was accepted as a single FRNT server IP")
	}
}
