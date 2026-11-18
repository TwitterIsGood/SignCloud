package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCacheDir(t *testing.T, dir string, size int, used time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "last-used")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, used, used); err != nil {
		t.Fatal(err)
	}
}

func TestCacheBudgetSplitsTotal(t *testing.T) {
	budget := (&Agent{cfg: Config{CacheMaxGB: 100}}).cacheBudget()
	if budget.compilationCache != 25*gib || budget.blobs != 10*gib || budget.projects != 65*gib {
		t.Fatalf("unexpected default budget: %+v", budget)
	}
	budget = (&Agent{cfg: Config{CacheMaxGB: 100, CompilationCacheMaxGB: 40, BlobCacheMaxGB: 5}}).cacheBudget()
	if budget.compilationCache != 40*gib || budget.blobs != 5*gib || budget.projects != 55*gib {
		t.Fatalf("unexpected explicit budget: %+v", budget)
	}
}

func TestCleanupCompilationCachesDropsExpiredThenOldest(t *testing.T) {
	root := t.TempDir()
	agent := &Agent{cfg: Config{CacheRoot: root}}
	now := time.Now()
	toolchains := filepath.Join(root, "toolchains")
	writeCacheDir(t, filepath.Join(toolchains, "expired"), 10, now.Add(-30*24*time.Hour))
	writeCacheDir(t, filepath.Join(toolchains, "old"), 600, now.Add(-2*time.Hour))
	writeCacheDir(t, filepath.Join(toolchains, "new"), 600, now)

	agent.cleanupCompilationCaches(now.Add(-14*24*time.Hour), 1000)

	for name, wantExists := range map[string]bool{"expired": false, "old": false, "new": true} {
		_, err := os.Stat(filepath.Join(toolchains, name))
		if exists := err == nil; exists != wantExists {
			t.Fatalf("%s exists=%v, want %v", name, exists, wantExists)
		}
	}
}

func TestCleanupBlobsEvictsLeastRecentlyUsed(t *testing.T) {
	root := t.TempDir()
	agent := &Agent{cfg: Config{CacheRoot: root}}
	now := time.Now()
	dir := filepath.Join(root, "blobs", "ab")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(name string, used time.Time) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, used, used); err != nil {
			t.Fatal(err)
		}
	}
	write("oldest", now.Add(-3*time.Hour))
	write("middle", now.Add(-2*time.Hour))
	write("newest", now)
	write(".download-partial", now.Add(-4*time.Hour))

	report := agent.cleanupBlobs(now.Add(-24*time.Hour), 200)

	if report.freed != 100 {
		t.Fatalf("freed %d bytes, want 100", report.freed)
	}
	for name, wantExists := range map[string]bool{"oldest": false, "middle": true, "newest": true, ".download-partial": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if exists := err == nil; exists != wantExists {
			t.Fatalf("%s exists=%v, want %v", name, exists, wantExists)
		}
	}
}

func TestCleanupProjectsKeepsCurrentWorkspace(t *testing.T) {
	root := t.TempDir()
	agent := &Agent{cfg: Config{CacheRoot: root}}
	now := time.Now()
	projects := filepath.Join(root, "projects")
	current := filepath.Join(projects, "p1", "tc")
	other := filepath.Join(projects, "p2", "tc")
	writeCacheDir(t, current, 800, now.Add(-5*time.Hour))
	writeCacheDir(t, other, 800, now)

	agent.cleanupProjects(current, now.Add(-24*time.Hour), 1000)

	if _, err := os.Stat(current); err != nil {
		t.Fatal("current workspace must never be evicted")
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatal("over-budget workspace should have been evicted")
	}
}

func TestRemoveCacheDirHandlesReadOnlyDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	locked := filepath.Join(root, "mod", "pkg")
	if err := os.MkdirAll(locked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file.go"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := removeCacheDir(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("cache directory still exists")
	}
}
