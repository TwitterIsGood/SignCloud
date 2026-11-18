package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeProject(t *testing.T, root, pbxproj string) {
	t.Helper()
	dir := filepath.Join(root, "App.xcodeproj")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.pbxproj"), []byte(pbxproj), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFastProfileSimulatorSettings(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, "\t\t\t\tONLY_ACTIVE_ARCH = NO;\n\t\t\t\tDEBUG_INFORMATION_FORMAT = dwarf;\n")
	runner := &jobRunner{}
	spec := BuildSpec{BuildProfile: "fast", SimulatorArch: "arm64"}

	got := runner.decideProfile(root, spec, "Debug", profileSimulator).settings
	want := []string{"COMPILER_INDEX_STORE_ENABLE=NO", "ARCHS=arm64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %v, want %v", got, want)
	}
	if got := runner.decideProfile(root, spec, "Debug", profileDevice).settings; !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("device settings = %v, want only the index setting", got)
	}
}

func TestFastProfileSkipsReleaseAndStandard(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, "")
	runner := &jobRunner{}
	if got := runner.decideProfile(root, BuildSpec{BuildProfile: "fast", SimulatorArch: "arm64"}, "Release", profileSimulator).settings; len(got) != 0 {
		t.Fatalf("Release must not get profile settings: %v", got)
	}
	if got := runner.decideProfile(root, BuildSpec{SimulatorArch: "arm64"}, "Debug", profileSimulator).settings; len(got) != 0 {
		t.Fatalf("no profile must mean no settings: %v", got)
	}
}

func TestFastProfileKeepsDeclaredSettings(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, "\t\t\t\tCOMPILER_INDEX_STORE_ENABLE = YES;\n\t\t\t\t\"EXCLUDED_ARCHS[sdk=iphonesimulator*]\" = arm64;\n")
	runner := &jobRunner{}
	decision := runner.decideProfile(root, BuildSpec{BuildProfile: "fast", SimulatorArch: "arm64"}, "Debug", profileSimulator)
	if len(decision.settings) != 0 {
		t.Fatalf("declared settings must win: %v", decision.settings)
	}
	if len(decision.notes) != 2 {
		t.Fatalf("expected two notes, got %v", decision.notes)
	}
}

func TestFastProfileReadsXcconfigArchs(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, "")
	if err := os.WriteFile(filepath.Join(root, "Debug.xcconfig"), []byte("ARCHS = $(ARCHS_STANDARD)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decision := (&jobRunner{}).decideProfile(root, BuildSpec{BuildProfile: "fast", SimulatorArch: "x86_64"}, "Debug", profileSimulator)
	if !reflect.DeepEqual(decision.settings, []string{"COMPILER_INDEX_STORE_ENABLE=NO"}) {
		t.Fatalf("ARCHS from xcconfig must be kept: %v", decision.settings)
	}
}
