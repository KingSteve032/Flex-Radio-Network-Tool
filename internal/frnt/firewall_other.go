//go:build !windows

package frnt

type FirewallCheck struct {
	Exists           bool
	ProgramMatches   bool
	InboundRuleFound bool
	InboundRuleOK    bool
	RawOutput        string
}

func (c *FirewallCheck) IsOK() bool {
	return c != nil && c.InboundRuleOK
}

func CheckFirewallRule(exePath string) (*FirewallCheck, error) {
	return &FirewallCheck{
		Exists:           true,
		ProgramMatches:   true,
		InboundRuleFound: true,
		InboundRuleOK:    true,
		RawOutput:        "firewall checks are only enforced on Windows",
	}, nil
}

func EnsureFirewallRule(exePath string) error {
	return nil
}
