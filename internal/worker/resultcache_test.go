package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDigestManifestIgnoresFileOrder(t *testing.T) {
	first := ProjectManifest{Version: 1, CacheKey: "project", Files: []ManifestFile{
		{Path: "B.swift", Hash: "b", Size: 2},
		{Path: "A.swift", Hash: "a", Size: 1},
	}}
	second := ProjectManifest{Version: 1, CacheKey: "project", Files: []ManifestFile{
		{Path: "A.swift", Hash: "a", Size: 1},
		{Path: "B.swift", Hash: "b", Size: 2},
	}}
	if digestManifest(first) != digestManifest(second) {
		t.Fatal("manifest digest depends on file order")
	}
}

func TestResultCacheEligibilityRequiresLockedDependencies(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		files    map[string]string
		eligible bool
	}{
		{name: "plain native", kind: "xcode", files: map[string]string{"App.xcodeproj/project.pbxproj": ""}, eligible: true},
		{name: "locked SwiftPM", kind: "xcode", files: map[string]string{
			"App.xcodeproj/project.pbxproj":                                           "XCRemoteSwiftPackageReference",
			"App.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved": "{}",
		}, eligible: true},
		{name: "unlocked SwiftPM", kind: "xcode", files: map[string]string{
			"App.xcodeproj/project.pbxproj": "XCRemoteSwiftPackageReference",
		}, eligible: false},
		{name: "locked CocoaPods", kind: "xcode", files: map[string]string{"Podfile": "", "Podfile.lock": ""}, eligible: true},
		{name: "unlocked CocoaPods", kind: "xcode", files: map[string]string{"Podfile": ""}, eligible: false},
		{name: "locked Expo", kind: "expo", files: map[string]string{"package.json": "{}", "package-lock.json": "{}"}, eligible: true},
		{name: "unlocked Expo", kind: "expo", files: map[string]string{"package.json": "{}"}, eligible: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := &cacheWorkspace{Source: root}
			for path, content := range test.files {
				fullPath := filepath.Join(root, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(fullPath), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fullPath, []byte(content), 0o640); err != nil {
					t.Fatal(err)
				}
				workspace.ManifestFiles = append(workspace.ManifestFiles, ManifestFile{Path: path})
			}
			eligible, _ := resultCacheEligible(workspace, test.kind)
			if eligible != test.eligible {
				t.Fatalf("eligible=%v, want %v", eligible, test.eligible)
			}
		})
	}
}

func TestStoreAndLoadCachedResult(t *testing.T) {
	root := t.TempDir()
	workspace := &cacheWorkspace{root: root}
	artifact := filepath.Join(root, "App.ipa")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata := ArtifactMetadata{BundleID: "com.example.app", Version: "1.0", MinimumOS: "16.0"}
	if err := storeCachedResult(workspace, "fingerprint", artifact, metadata); err != nil {
		t.Fatal(err)
	}
	path, loaded, hit := cachedResult(workspace, "fingerprint")
	if !hit || filepath.Base(path) != "App.ipa" || !reflect.DeepEqual(loaded, metadata) {
		t.Fatalf("unexpected cached result: hit=%v path=%q metadata=%+v", hit, path, loaded)
	}
	if _, _, hit := cachedResult(workspace, "different"); hit {
		t.Fatal("cache hit for a different fingerprint")
	}
}

func TestResultFingerprintIncludesSpecAndToolchain(t *testing.T) {
	agent := &Agent{toolchains: map[string]string{"": "xcode-test"}}
	workspace := &cacheWorkspace{ManifestDigest: "manifest"}
	build := Build{Kind: "simulator"}
	first, err := agent.resultFingerprint(workspace, build, BuildSpec{Scheme: "App", Configuration: "Debug"}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.resultFingerprint(workspace, build, BuildSpec{Scheme: "App", Configuration: "Release"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different build specs produced the same fingerprint")
	}
}

func TestResultFingerprintIncludesMacOSArtifactNameOnly(t *testing.T) {
	agent := &Agent{toolchains: map[string]string{"": "xcode-test"}}
	workspace := &cacheWorkspace{ManifestDigest: "manifest"}
	spec := BuildSpec{Scheme: "App", Project: "App.xcodeproj", Signing: "adhoc"}
	first, err := agent.resultFingerprint(workspace, Build{Kind: "macos", Name: "First"}, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.resultFingerprint(workspace, Build{Kind: "macos", Name: "Second"}, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("macOS artifact display name was omitted from the fingerprint")
	}
	iOSFirst, _ := agent.resultFingerprint(workspace, Build{Kind: "simulator", Name: "First"}, spec, "")
	iOSSecond, _ := agent.resultFingerprint(workspace, Build{Kind: "simulator", Name: "Second"}, spec, "")
	if iOSFirst != iOSSecond {
		t.Fatal("iOS cache compatibility changed when only the display name changed")
	}
}
