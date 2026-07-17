package frnt

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestBuildGitHubReleaseInstallCommandUsesSafeInstallHelper(t *testing.T) {
	cmd := buildGitHubReleaseInstallCommand("", "")

	for _, want := range []string{
		"set -eu",
		"install_path='/usr/local/bin/frnt'",
		"sudo_password_b64=\"" + sudoPasswordB64Token + "\"",
		"install_frnt_binary()",
		"install_frnt_binary \"$tmp\" \"$install_path\"",
		"Install source is missing",
		"Downloading ${asset} from ${url}",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("generated command missing %q\n%s", want, cmd)
		}
	}

	if strings.Contains(cmd, "install -m 0755 \"$tmp\" '/usr/local/bin/frnt'") {
		t.Fatalf("generated command still installs directly from shell variable\n%s", cmd)
	}
}

func TestApplyTargetSudoPasswordToken(t *testing.T) {
	cmd := buildGitHubReleaseInstallCommand("", "")

	withoutPassword := applyTargetSudoPasswordToken(cmd, "")
	if strings.Contains(withoutPassword, sudoPasswordB64Token) {
		t.Fatalf("sudo token was not removed:\n%s", withoutPassword)
	}
	if !strings.Contains(withoutPassword, "sudo_password_b64=\"\"") {
		t.Fatalf("empty sudo password did not produce empty script variable:\n%s", withoutPassword)
	}

	withPassword := applyTargetSudoPasswordToken(cmd, "secret")
	want := "sudo_password_b64=\"" + base64.StdEncoding.EncodeToString([]byte("secret")) + "\""
	if !strings.Contains(withPassword, want) {
		t.Fatalf("sudo token was not replaced with encoded password %q:\n%s", want, withPassword)
	}
}

func TestBuildConfigApplyCommandInstallsSystemConfig(t *testing.T) {
	cmd := buildConfigApplyCommand(adminFlexToolConfig{ListenInterface: "eth0", SendInterface: "wt0"}, "secret")

	for _, want := range []string{
		"LISTEN_INTERFACE=eth0",
		"SEND_INTERFACE=wt0",
		"mkdir -p",
		"/etc/frnt",
		"install -m 0600",
		"/tmp/frnt-flextool",
		"/etc/frnt/flextool",
		"install -m 0600 '/tmp/frnt-flextool' \"$HOME/.flextool\"",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("config apply command missing %q\n%s", want, cmd)
		}
	}

	for _, oldProxyKey := range []string{"PROXY_BASE_PORT", "PROXY_LAN_SOURCE_IPS", "MULTI_PROXY"} {
		if strings.Contains(cmd, oldProxyKey) {
			t.Fatalf("config apply command still writes old proxy key %q\n%s", oldProxyKey, cmd)
		}
	}
}

func TestBuildInstallServiceCommandLoadsSystemConfig(t *testing.T) {
	cmd := buildInstallServiceCommand("", "", "")

	for _, want := range []string{
		"EnvironmentFile=-/etc/frnt/flextool",
		"ExecStart=/usr/local/bin/frnt --mode server --config /etc/frnt/flextool listen",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("service install command missing %q\n%s", want, cmd)
		}
	}
}
