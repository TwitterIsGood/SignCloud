package worker

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnzipRejectsPathTraversal(t *testing.T) {
	temp := t.TempDir()
	archive := filepath.Join(temp, "bad.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("../outside.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("bad"))
	_ = writer.Close()
	_ = file.Close()

	if err := unzip(archive, filepath.Join(temp, "out")); err == nil {
		t.Fatal("expected unsafe archive path to be rejected")
	}
}

func TestUnzipRestoresSymlinkEntries(t *testing.T) {
	temp := t.TempDir()
	archive := filepath.Join(temp, "links.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	header := &zip.FileHeader{Name: "Lib.framework/Lib"}
	header.SetMode(os.ModeSymlink | 0o777)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("Versions/Current/Lib")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	output := filepath.Join(temp, "out")
	if err := unzip(archive, output); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(output, "Lib.framework", "Lib")
	target, err := os.Readlink(link)
	if err != nil || target != "Versions/Current/Lib" {
		t.Fatalf("readlink = %q, %v; want Versions/Current/Lib", target, err)
	}
}

func TestChildEnvironmentRemovesLaunchdXPCMarkers(t *testing.T) {
	t.Setenv("XPC_SERVICE_NAME", "com.signcloud.agent")
	t.Setenv("XPC_FLAGS", "1")
	environment := childEnvironment([]string{"EXAMPLE=value"})
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "XPC_SERVICE_NAME=") || strings.Contains(joined, "XPC_FLAGS=") {
		t.Fatal("launchd XPC markers leaked into child environment")
	}
	if !strings.Contains(joined, "EXAMPLE=value") {
		t.Fatal("child environment override is missing")
	}
}

func TestFastPackageEligibility(t *testing.T) {
	runner := &jobRunner{agent: &Agent{cfg: Config{ExportMethod: "development"}}}
	tests := []struct {
		name     string
		spec     BuildSpec
		eligible bool
	}{
		{name: "default development debug", spec: BuildSpec{}, eligible: true},
		{name: "explicit development debug", spec: BuildSpec{ExportMethod: "development", Configuration: "Debug"}, eligible: true},
		{name: "release configuration", spec: BuildSpec{ExportMethod: "development", Configuration: "Release"}, eligible: false},
		{name: "ad hoc", spec: BuildSpec{ExportMethod: "ad-hoc", Configuration: "Debug"}, eligible: false},
		{name: "app store", spec: BuildSpec{ExportMethod: "app-store", Configuration: "Release"}, eligible: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			eligible, _ := runner.fastPackageEligible(test.spec)
			if eligible != test.eligible {
				t.Fatalf("eligible=%v, want %v", eligible, test.eligible)
			}
		})
	}
}

func TestWildcardSigningEligible(t *testing.T) {
	tests := []struct {
		name     string
		project  string
		eligible bool
	}{
		{name: "single application", project: `productType = "com.apple.product-type.application";`, eligible: true},
		{name: "extension", project: `productType = "com.apple.product-type.application"; productType = "com.apple.product-type.app-extension";`, eligible: false},
		{name: "entitlements", project: `productType = "com.apple.product-type.application"; CODE_SIGN_ENTITLEMENTS = App.entitlements;`, eligible: false},
		{name: "ios scoped entitlements", project: `productType = "com.apple.product-type.application"; "CODE_SIGN_ENTITLEMENTS[sdk=iphoneos*]" = App.entitlements;`, eligible: false},
		{name: "macos scoped entitlements", project: `productType = "com.apple.product-type.application"; "CODE_SIGN_ENTITLEMENTS[sdk=macosx*]" = App.entitlements;`, eligible: true},
		{name: "multiple applications", project: `productType = "com.apple.product-type.application"; productType = "com.apple.product-type.application";`, eligible: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "App.xcodeproj")
			if err := os.MkdirAll(project, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "project.pbxproj"), []byte(test.project), 0o600); err != nil {
				t.Fatal(err)
			}
			eligible, _ := wildcardSigningEligible(project)
			if eligible != test.eligible {
				t.Fatalf("eligible=%v, want %v", eligible, test.eligible)
			}
		})
	}
}

func TestProvisioningProfileName(t *testing.T) {
	plist := []byte("<?xml version=\"1.0\"?>\n<plist><dict>\n<key>AppIDName</key>\n<string>XC Wildcard</string>\n<key>Name</key>\n<string>SignCloud Wildcard Development 20261020</string>\n</dict></plist>")
	if got := provisioningProfileName(plist); got != "SignCloud Wildcard Development 20261020" {
		t.Fatalf("got %q", got)
	}
	if got := provisioningProfileName([]byte("<plist></plist>")); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestXMLescape(t *testing.T) {
	got := xmlEscape(`A&B<"'>`)
	want := "A&amp;B&lt;&quot;&apos;&gt;"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCurrentExportMethod(t *testing.T) {
	tests := map[string]string{
		"development": "debugging",
		"ad-hoc":      "release-testing",
		"app-store":   "app-store-connect",
		"enterprise":  "enterprise",
	}
	for input, want := range tests {
		if got := currentExportMethod(input); got != want {
			t.Fatalf("currentExportMethod(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUniqueSharedScheme(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "App.xcodeproj")
	schemeDir := filepath.Join(project, "xcshareddata", "xcschemes")
	if err := os.MkdirAll(schemeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemeDir, "App.xcscheme"), []byte("scheme"), 0o600); err != nil {
		t.Fatal(err)
	}

	if scheme, ok := uniqueSharedScheme(root, []string{"-project", project}); !ok || scheme != "App" {
		t.Fatalf("project scheme = %q, %v; want App, true", scheme, ok)
	}
	if scheme, ok := uniqueSharedScheme(root, []string{"-workspace", filepath.Join(root, "App.xcworkspace")}); !ok || scheme != "App" {
		t.Fatalf("workspace scheme = %q, %v; want App, true", scheme, ok)
	}

	second := filepath.Join(root, "Second.xcodeproj", "xcshareddata", "xcschemes")
	if err := os.MkdirAll(second, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "Second.xcscheme"), []byte("scheme"), 0o600); err != nil {
		t.Fatal(err)
	}
	if scheme, ok := uniqueSharedScheme(root, []string{"-workspace", filepath.Join(root, "App.xcworkspace")}); ok {
		t.Fatalf("ambiguous workspace scheme = %q, true; want fallback", scheme)
	}
}

func TestUniqueSharedSchemeIgnoresDependencySchemes(t *testing.T) {
	root := t.TempDir()
	appScheme := filepath.Join(root, "App.xcodeproj", "xcshareddata", "xcschemes")
	podScheme := filepath.Join(root, "Pods", "Pods.xcodeproj", "xcshareddata", "xcschemes")
	for _, directory := range []string{appScheme, podScheme} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(appScheme, "App.xcscheme"), []byte("scheme"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podScheme, "Dependency.xcscheme"), []byte("scheme"), 0o600); err != nil {
		t.Fatal(err)
	}

	if scheme, ok := uniqueSharedScheme(root, []string{"-workspace", filepath.Join(root, "App.xcworkspace")}); !ok || scheme != "App" {
		t.Fatalf("workspace scheme = %q, %v; want App, true", scheme, ok)
	}
}

func TestPackageResolvedCollectionAndZip(t *testing.T) {
	root := t.TempDir()
	wantedRelative := filepath.ToSlash(filepath.Join("App.xcodeproj", "project.xcworkspace", "xcshareddata", "swiftpm", "Package.resolved"))
	wanted := filepath.Join(root, filepath.FromSlash(wantedRelative))
	if err := os.MkdirAll(filepath.Dir(wanted), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wanted, []byte(`{"pins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, skipped := range []string{
		filepath.Join(root, "DerivedData", "SourcePackages", "Package.resolved"),
		filepath.Join(root, "Pods", "Package.resolved"),
	} {
		if err := os.MkdirAll(filepath.Dir(skipped), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(skipped, []byte(`{"skipped":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, err := packageResolvedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != wanted {
		t.Fatalf("packageResolvedFiles = %v, want [%s]", files, wanted)
	}

	output := filepath.Join(t.TempDir(), "Package.resolved.zip")
	if err := zipPackageResolved(root, files, output); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.File) != 1 || reader.File[0].Name != wantedRelative {
		t.Fatalf("zip entries = %v, want [%s]", reader.File, wantedRelative)
	}
}
