package control

import (
	"strings"
	"testing"
)

func TestValidateMacOSSpec(t *testing.T) {
	valid := `{"scheme":"App","project":"App.xcodeproj","team_id":"TEAM123456","architectures":["arm64","x86_64"],"signing":"developer-id","notarize":true,"artifact_format":"dmg"}`
	if _, err := parseAndValidateBuildSpec("macos", []byte(valid)); err != nil {
		t.Fatalf("valid macOS spec was rejected: %v", err)
	}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"both containers", `{"scheme":"App","project":"App.xcodeproj","workspace":"App.xcworkspace"}`, "exactly one"},
		{"unsafe path", `{"scheme":"App","project":"../App.xcodeproj"}`, "safe relative"},
		{"unknown command", `{"scheme":"App","project":"App.xcodeproj","prebuild":"curl bad"}`, "unknown field"},
		{"environment", `{"scheme":"App","project":"App.xcodeproj","environment":{"PATH":"bad"}}`, "environment"},
		{"adhoc notarization", `{"scheme":"App","project":"App.xcodeproj","signing":"adhoc","notarize":true}`, "requires signing=developer-id"},
		{"unsupported architecture", `{"scheme":"App","project":"App.xcodeproj","architectures":["ppc"]}`, "unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseAndValidateBuildSpec("macos", []byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestMacOSOptionsDoNotChangeIOSSpecCompatibility(t *testing.T) {
	if _, err := parseAndValidateBuildSpec("xcode", []byte(`{"scheme":"App","environment":{"SAFE":"legacy"},"legacy_extra":true}`)); err != nil {
		t.Fatalf("legacy iOS spec compatibility regressed: %v", err)
	}
}

func TestValidateXcodeVersion(t *testing.T) {
	for _, valid := range []string{`{"xcode_version":"27"}`, `{"xcode_version":"26.6"}`, `{"xcode_version":"27.0.1"}`} {
		if _, err := parseAndValidateBuildSpec("xcode", []byte(valid)); err != nil {
			t.Fatalf("valid xcode spec %s was rejected: %v", valid, err)
		}
	}
	if _, err := parseAndValidateBuildSpec("macos", []byte(`{"scheme":"App","project":"App.xcodeproj","xcode_version":"27"}`)); err != nil {
		t.Fatalf("valid macOS xcode_version was rejected: %v", err)
	}
	for _, invalid := range []string{`{"xcode_version":"27-beta"}`, `{"xcode_version":"/Applications/Xcode.app"}`, `{"xcode_version":"27."}`} {
		if _, err := parseAndValidateBuildSpec("xcode", []byte(invalid)); err == nil {
			t.Fatalf("invalid xcode_version %s was accepted", invalid)
		}
	}
	if _, err := parseAndValidateBuildSpec("macos", []byte(`{"scheme":"App","project":"App.xcodeproj","xcode_version":"latest"}`)); err == nil {
		t.Fatal("invalid macOS xcode_version was accepted")
	}
}

func TestValidateResolveSpec(t *testing.T) {
	for _, valid := range []string{`{}`, `{"project":"App.xcodeproj"}`, `{"workspace":"App.xcworkspace"}`} {
		if _, err := parseAndValidateBuildSpec("resolve", []byte(valid)); err != nil {
			t.Fatalf("valid resolve spec %s was rejected: %v", valid, err)
		}
	}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"both containers", `{"project":"App.xcodeproj","workspace":"App.xcworkspace"}`, "at most one"},
		{"unsafe path", `{"project":"../App.xcodeproj"}`, "safe relative"},
		{"environment", `{"environment":{"PATH":"bad"}}`, "environment"},
		{"unknown field", `{"command":"curl bad"}`, "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseAndValidateBuildSpec("resolve", []byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}
