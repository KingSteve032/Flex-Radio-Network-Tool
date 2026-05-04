//go:build windows

package frnt

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	firewallRulePrimaryName  = "Flex Radio Network Tool"
	firewallRuleBaseName     = "Flex Radio Network Tool"
	firewallRuleInboundName  = "Flex Radio Network Tool (Inbound UDP)"
	firewallRuleOutboundName = "Flex Radio Network Tool (Outbound UDP)"
)

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

type netshFirewallRule struct {
	Name      string
	Direction string
	Action    string
	Enabled   string
	Program   string
}

// CheckFirewallRule checks whether a firewall rule exists and whether its "Program:" matches exePath.
// This should NOT require admin.
//
// IMPORTANT: netsh sometimes returns a non-zero exit code even when it prints valid rule output.
// We treat the output as authoritative if it contains real rule blocks ("Rule Name:").
func CheckFirewallRule(exePath string) (*FirewallCheck, error) {
	parsedRules, raw, err := getManagedFirewallRules()
	if err != nil {
		return &FirewallCheck{
			Exists:           false,
			ProgramMatches:   false,
			InboundRuleFound: false,
			InboundRuleOK:    false,
			RawOutput:        raw,
		}, err
	}

	if len(parsedRules) == 0 {
		return &FirewallCheck{
			Exists:         false,
			ProgramMatches: false,
			RawOutput:      raw,
		}, nil
	}

	check := &FirewallCheck{
		Exists:    true,
		RawOutput: raw,
	}

	for _, rule := range parsedRules {
		direction := normalizeRuleDirection(rule.Direction, rule.Name)
		if direction == "" {
			continue
		}

		pathMatches := programMatchesExpected(rule.Program, exePath)
		ruleOK := pathMatches &&
			isAllowAction(rule.Action) &&
			isEnabledRule(rule.Enabled)

		switch direction {
		case "in":
			check.InboundRuleFound = true
			if pathMatches {
				check.ProgramMatches = true
			}
			if ruleOK {
				check.InboundRuleOK = true
			}
		}
	}

	return check, nil
}

func getManagedFirewallRules() ([]netshFirewallRule, string, error) {
	raw, err := runNetsh(15*time.Second, "advfirewall", "firewall", "show", "rule", "name=all", "verbose")
	lower := strings.ToLower(raw)

	if strings.Contains(lower, "no rules match") {
		return nil, raw, nil
	}

	rules := parseNetshFirewallRules(raw)
	if err != nil && len(rules) == 0 {
		return nil, raw, fmt.Errorf("netsh show rule failed: %w", err)
	}

	return filterManagedRules(rules), raw, nil
}

func filterManagedRules(rules []netshFirewallRule) []netshFirewallRule {
	out := make([]netshFirewallRule, 0, len(rules))
	for _, r := range rules {
		if isManagedRuleName(r.Name) {
			out = append(out, r)
		}
	}
	return out
}

func isManagedRuleName(name string) bool {
	n := normalizeRuleName(name)
	primary := normalizeRuleName(firewallRulePrimaryName)
	base := normalizeRuleName(firewallRuleBaseName)

	if n == primary ||
		n == base ||
		n == normalizeRuleName(firewallRuleInboundName) ||
		n == normalizeRuleName(firewallRuleOutboundName) {
		return true
	}

	// Accept legacy variants like "... (Inbound UDP)".
	return strings.HasPrefix(n, base+" (") && strings.HasSuffix(n, ")")
}

func programMatchesExpected(ruleProgram, expectedProgram string) bool {
	ruleRaw := strings.Trim(strings.TrimSpace(ruleProgram), `"`)
	expRaw := strings.Trim(strings.TrimSpace(expectedProgram), `"`)

	ruleNorm := canonicalProgramPath(ruleRaw)
	expNorm := canonicalProgramPath(expRaw)
	if ruleNorm == "" || expNorm == "" {
		return false
	}
	if ruleNorm == expNorm {
		return true
	}

	// Best-effort canonical match if both paths resolve to the same file.
	rInfo, rErr := os.Stat(ruleRaw)
	eInfo, eErr := os.Stat(expRaw)
	if rErr == nil && eErr == nil && os.SameFile(rInfo, eInfo) {
		return true
	}

	return false
}

func canonicalProgramPath(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `"`)
	if v == "" {
		return ""
	}

	p := filepath.Clean(v)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if eval, err := filepath.EvalSymlinks(p); err == nil {
		p = eval
	}
	return strings.ToLower(p)
}

func normalizeRuleName(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `"`)
	return strings.ToLower(v)
}

func parseNetshFirewallRules(raw string) []netshFirewallRule {
	scanner := bufio.NewScanner(strings.NewReader(raw))

	var (
		rules   []netshFirewallRule
		current *netshFirewallRule
	)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if v, ok := readNetshField(line, "rule name:"); ok {
			if current != nil {
				rules = append(rules, *current)
			}
			current = &netshFirewallRule{Name: v}
			continue
		}
		if current == nil {
			continue
		}

		if v, ok := readNetshField(line, "direction:"); ok {
			current.Direction = v
			continue
		}
		if v, ok := readNetshField(line, "action:"); ok {
			current.Action = v
			continue
		}
		if v, ok := readNetshField(line, "enabled:"); ok {
			current.Enabled = v
			continue
		}
		if v, ok := readNetshField(line, "program:"); ok {
			current.Program = v
			continue
		}
	}

	if current != nil {
		rules = append(rules, *current)
	}
	return rules
}

func readNetshField(line, field string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, field) {
		return "", false
	}
	return strings.TrimSpace(trimmed[len(field):]), true
}

func normalizeRuleDirection(v, name string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case strings.HasPrefix(v, "in"):
		return "in"
	case strings.HasPrefix(v, "out"):
		return "out"
	}

	n := strings.ToLower(name)
	if strings.Contains(n, "(inbound") {
		return "in"
	}
	if strings.Contains(n, "(outbound") {
		return "out"
	}
	return ""
}

func isAllowAction(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "allow")
}

func isEnabledRule(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "yes" || v == "true"
}

// EnsureFirewallRule ensures inbound allow rule exists for this exePath.
// This WILL prompt for UAC (admin) because netsh add/delete requires elevation.
func EnsureFirewallRule(exePath string) error {
	// Common failure: EXE on a UNC/network path.
	if strings.HasPrefix(strings.ToLower(exePath), `\\`) {
		return fmt.Errorf(
			"executable is on a network path (UNC): %s\nMove the EXE to a local drive (e.g. C:\\Tools\\frnt.exe) and try again.",
			exePath,
		)
	}

	managedRules, _, listErr := getManagedFirewallRules()
	if listErr != nil {
		return fmt.Errorf("failed enumerating existing firewall rules: %w", listErr)
	}

	namesToDelete := make(map[string]struct{})
	for _, n := range []string{
		firewallRulePrimaryName,
		firewallRuleBaseName,
		firewallRuleInboundName,
		firewallRuleOutboundName,
	} {
		n = strings.TrimSpace(n)
		if n != "" {
			namesToDelete[n] = struct{}{}
		}
	}
	for _, r := range managedRules {
		n := strings.TrimSpace(r.Name)
		if n != "" {
			namesToDelete[n] = struct{}{}
		}
	}

	deleteNames := make([]string, 0, len(namesToDelete))
	for n := range namesToDelete {
		deleteNames = append(deleteNames, n)
	}
	sort.Strings(deleteNames)

	out, err := runFirewallUpdateElevatedCaptured(exePath, deleteNames)
	if err != nil {
		if isUACCancelledOutput(out) {
			return fmt.Errorf("firewall update was cancelled at the Windows UAC prompt")
		}
		return fmt.Errorf("failed applying firewall rule changes: %w\n\nnetsh output:\n%s", err, out)
	}

	chk, chkErr := CheckFirewallRule(exePath)
	if chkErr != nil {
		raw := ""
		if chk != nil {
			raw = chk.RawOutput
		}
		return fmt.Errorf("failed verifying firewall rule after update: %w\n\nnetsh output:\n%s", chkErr, raw)
	}
	if chk == nil || !chk.IsOK() {
		return fmt.Errorf(
			"firewall rule verification failed after update (inbound_ok=%t)",
			chk != nil && chk.InboundRuleOK,
		)
	}

	return nil
}

func runFirewallUpdateElevatedCaptured(exePath string, deleteRuleNames []string) (string, error) {
	tmpDir := os.TempDir()
	scriptFile := filepath.Join(tmpDir, fmt.Sprintf("frnt-fw-script-%d.ps1", time.Now().UnixNano()))
	outFile := filepath.Join(tmpDir, fmt.Sprintf("frnt-fw-out-%d.txt", time.Now().UnixNano()))
	defer func() {
		_ = os.Remove(scriptFile)
		_ = os.Remove(outFile)
	}()

	var elevated bytes.Buffer
	elevated.WriteString("$ErrorActionPreference = 'Continue'\n")
	elevated.WriteString(fmt.Sprintf("$outFile = '%s'\n", strings.ReplaceAll(outFile, `'`, `''`)))
	elevated.WriteString("'' | Out-File -FilePath $outFile -Encoding UTF8\n")
	elevated.WriteString("function Invoke-Netsh([string[]]$argsList) {\n")
	elevated.WriteString("  $cmd = 'netsh ' + ($argsList -join ' ')\n")
	elevated.WriteString("  ('> ' + $cmd) | Out-File -FilePath $outFile -Append -Encoding UTF8\n")
	elevated.WriteString("  $result = & netsh.exe @argsList 2>&1\n")
	elevated.WriteString("  if ($null -ne $result) { $result | Out-File -FilePath $outFile -Append -Encoding UTF8 }\n")
	elevated.WriteString("  return $LASTEXITCODE\n")
	elevated.WriteString("}\n")

	for _, ruleName := range deleteRuleNames {
		arg := strings.ReplaceAll(fmt.Sprintf(`name="%s"`, ruleName), `'`, `''`)
		elevated.WriteString(fmt.Sprintf("$null = Invoke-Netsh @('advfirewall','firewall','delete','rule','%s')\n", arg))
	}

	addNameArg := strings.ReplaceAll(fmt.Sprintf(`name="%s"`, firewallRulePrimaryName), `'`, `''`)
	addProgramArg := strings.ReplaceAll(fmt.Sprintf(`program="%s"`, exePath), `'`, `''`)
	elevated.WriteString(fmt.Sprintf("$code = Invoke-Netsh @('advfirewall','firewall','add','rule','%s','dir=in','action=allow','%s','enable=yes','profile=any')\n", addNameArg, addProgramArg))
	elevated.WriteString("if ($code -eq $null) { $code = 1 }\n")
	elevated.WriteString("exit $code\n")

	if err := os.WriteFile(scriptFile, []byte(elevated.String()), 0600); err != nil {
		return "", fmt.Errorf("failed writing elevated firewall script: %w", err)
	}

	var launcher bytes.Buffer
	launcher.WriteString(fmt.Sprintf("$scriptPath = '%s'\n", strings.ReplaceAll(scriptFile, `'`, `''`)))
	launcher.WriteString("try {\n")
	launcher.WriteString(`  $p = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoProfile","-ExecutionPolicy","Bypass","-File",$scriptPath) -Verb RunAs -Wait -PassThru` + "\n")
	launcher.WriteString("  exit $p.ExitCode\n")
	launcher.WriteString("} catch {\n")
	launcher.WriteString("  $_ | Out-String\n")
	launcher.WriteString("  exit 1\n")
	launcher.WriteString("}\n")

	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", launcher.String())
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	launcherOut, err := cmd.CombinedOutput()

	b, readErr := os.ReadFile(outFile)
	out := ""
	if readErr == nil {
		out = string(b)
	} else {
		out = fmt.Sprintf("(failed to read firewall output file: %v)", readErr)
	}

	launcherText := strings.TrimSpace(string(launcherOut))
	if launcherText != "" {
		if out != "" {
			out += "\n"
		}
		out += launcherText
	}

	return out, err
}

func isUACCancelledOutput(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "operation was canceled by the user") ||
		strings.Contains(lower, "operation was cancelled by the user") ||
		strings.Contains(lower, "the operation was canceled by the user")
}

// runNetsh runs netsh with a timeout (prevents UI from getting stuck).
func runNetsh(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	out, err := cmd.CombinedOutput()
	raw := string(out)

	if ctx.Err() == context.DeadlineExceeded {
		return raw, fmt.Errorf("netsh timed out")
	}
	return raw, err
}
