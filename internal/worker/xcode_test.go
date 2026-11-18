package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeXcode(t *testing.T, root, name, version string) string {
	t.Helper()
	app := filepath.Join(root, name)
	contents := filepath.Join(app, "Contents")
	if err := os.MkdirAll(filepath.Join(contents, "Developer"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>` + version + `</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(contents, "version.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestDiscoverXcodesOrdersNewestFirst(t *testing.T) {
	root := t.TempDir()
	writeFakeXcode(t, root, "Xcode_26.6.app", "26.6")
	writeFakeXcode(t, root, "Xcode_27.0.app", "27.0")

	installs, err := discoverXcodes([]string{filepath.Join(root, "Xcode*.app")})
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 2 {
		t.Fatalf("expected 2 installs, got %d", len(installs))
	}
	if installs[0].Version != "27.0" || installs[1].Version != "26.6" {
		t.Fatalf("unexpected order: %+v", installs)
	}
}

func TestXcodeDeveloperDirMatchesMajorAndExactVersions(t *testing.T) {
	root := t.TempDir()
	xcode26 := writeFakeXcode(t, root, "Xcode_26.6.app", "26.6")
	xcode27 := writeFakeXcode(t, root, "Xcode_27.0.app", "27.0")
	agent := &Agent{cfg: Config{XcodePaths: []string{filepath.Join(root, "Xcode*.app")}}}

	cases := map[string]string{
		"":     "",
		"26":   filepath.Join(xcode26, "Contents", "Developer"),
		"26.6": filepath.Join(xcode26, "Contents", "Developer"),
		"27":   filepath.Join(xcode27, "Contents", "Developer"),
		"27.0": filepath.Join(xcode27, "Contents", "Developer"),
	}
	for requested, wanted := range cases {
		got, err := agent.xcodeDeveloperDir(requested)
		if err != nil {
			t.Fatalf("xcode_version %q: %v", requested, err)
		}
		if got != wanted {
			t.Fatalf("xcode_version %q: got %q, want %q", requested, got, wanted)
		}
	}
}

func TestXcodeDeveloperDirReportsAvailableVersions(t *testing.T) {
	root := t.TempDir()
	writeFakeXcode(t, root, "Xcode_26.6.app", "26.6")
	agent := &Agent{cfg: Config{XcodePaths: []string{filepath.Join(root, "Xcode*.app")}}}

	_, err := agent.xcodeDeveloperDir("25")
	if err == nil {
		t.Fatal("expected an error for a missing Xcode version")
	}
	if !strings.Contains(err.Error(), "26.6") {
		t.Fatalf("error should list available versions: %v", err)
	}
}

func TestXcodeDeveloperDirRefreshesAfterInstall(t *testing.T) {
	root := t.TempDir()
	writeFakeXcode(t, root, "Xcode_26.6.app", "26.6")
	agent := &Agent{cfg: Config{XcodePaths: []string{filepath.Join(root, "Xcode*.app")}}}
	if _, err := agent.xcodeDeveloperDir("26"); err != nil {
		t.Fatal(err)
	}
	xcode27 := writeFakeXcode(t, root, "Xcode_27.0.app", "27.0")
	got, err := agent.xcodeDeveloperDir("27")
	if err != nil {
		t.Fatalf("newly installed Xcode was not discovered: %v", err)
	}
	if want := filepath.Join(xcode27, "Contents", "Developer"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWithXcodeVersionPinsDeveloperDir(t *testing.T) {
	runner := &jobRunner{developerDir: "/Applications/Xcode_27.app/Contents/Developer"}
	env := runner.withXcodeVersion([]string{"HOME=/tmp"})
	if len(env) != 2 || env[1] != "DEVELOPER_DIR=/Applications/Xcode_27.app/Contents/Developer" {
		t.Fatalf("unexpected environment: %v", env)
	}
	unchanged := (&jobRunner{}).withXcodeVersion([]string{"HOME=/tmp"})
	if len(unchanged) != 1 || unchanged[0] != "HOME=/tmp" {
		t.Fatalf("default Xcode should not add DEVELOPER_DIR: %v", unchanged)
	}
}
