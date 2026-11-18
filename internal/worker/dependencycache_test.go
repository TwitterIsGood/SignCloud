package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDependencyFingerprintChangesWithInputs(t *testing.T) {
	root := t.TempDir()
	packageJSON := filepath.Join(root, "package.json")
	lockFile := filepath.Join(root, "package-lock.json")
	if err := os.WriteFile(packageJSON, []byte(`{"name":"app"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, []byte(`{"lockfileVersion":3}`), 0o640); err != nil {
		t.Fatal(err)
	}
	first, err := dependencyFingerprint(root, []string{"package.json", "package-lock.json"}, "npm:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, []byte(`{"lockfileVersion":4}`), 0o640); err != nil {
		t.Fatal(err)
	}
	second, err := dependencyFingerprint(root, []string{"package.json", "package-lock.json"}, "npm:1")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("dependency fingerprint ignored lock file change")
	}
}

func TestDependencyCacheStateRequiresFingerprintAndPaths(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state.json")
	required := filepath.Join(root, "node_modules")
	if err := os.MkdirAll(required, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := storeDependencyCacheState(state, "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if !dependencyCacheHit(state, "fingerprint", required) {
		t.Fatal("expected dependency cache hit")
	}
	if dependencyCacheHit(state, "different", required) {
		t.Fatal("cache hit for a different dependency fingerprint")
	}
	if dependencyCacheHit(state, "fingerprint", filepath.Join(root, "missing")) {
		t.Fatal("cache hit with a missing dependency directory")
	}
}

func TestFilesEqual(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("same"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("same"), 0o640); err != nil {
		t.Fatal(err)
	}
	if !filesEqual(first, second) {
		t.Fatal("equal files were not recognized")
	}
	if err := os.WriteFile(second, []byte("different"), 0o640); err != nil {
		t.Fatal(err)
	}
	if filesEqual(first, second) {
		t.Fatal("different files were considered equal")
	}
}
