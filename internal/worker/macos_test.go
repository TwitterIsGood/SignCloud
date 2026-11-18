package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMacOSArchitectureDefaultsAndMissing(t *testing.T) {
	if got := normalizedMacOSArchitectures(nil); !reflect.DeepEqual(got, []string{"arm64", "x86_64"}) {
		t.Fatalf("default architectures = %v", got)
	}
	if got := missingArchitectures([]string{"arm64", "x86_64"}, []string{"arm64"}); !reflect.DeepEqual(got, []string{"x86_64"}) {
		t.Fatalf("missing architectures = %v", got)
	}
}

func TestVerifyMacOSBundledResources(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(t.TempDir(), "App.app")
	if err := os.MkdirAll(filepath.Join(root, "Resources", "zh-Hans.lproj"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app, "Contents", "Resources", "zh-Hans.lproj"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := verifyMacOSBundledResources(root, app); err != nil {
		t.Fatalf("bundled localization was rejected: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(app, "Contents", "Resources", "zh-Hans.lproj")); err != nil {
		t.Fatal(err)
	}
	if err := verifyMacOSBundledResources(root, app); err == nil {
		t.Fatal("missing localization was accepted")
	}
}

func TestSafeMacOSDisplayName(t *testing.T) {
	if got := safeMacOSDisplayName("无线/调试:助手.app"); got != "无线-调试-助手" {
		t.Fatalf("display name = %q", got)
	}
}

func TestWorkerRedactsCredentialMaterial(t *testing.T) {
	agent := &Agent{cfg: Config{
		AgentToken:                "top-secret-token",
		APIKeyPath:                "/private/credentials/notary.p8",
		MacOSKeychainPasswordFile: "/private/credentials/password",
	}}
	redacted := agent.redact("token=top-secret-token /private/credentials/notary.p8 /private/credentials/password")
	for _, secret := range []string{"top-secret-token", "/private/credentials/notary.p8", "/private/credentials/password"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("secret was not redacted: %q", redacted)
		}
	}
}
