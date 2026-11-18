package worker

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Cache budget
//
// SIGNCLOUD_CACHE_MAX_GB is the worker's total cache budget. It is split into
// three tiers that are evicted independently, because each has a different
// cost to rebuild:
//
//   - compilation cache (toolchains/*/CompilationCache.noindex): shared by all
//     projects on one toolchain; rebuilt by the next cold compile.
//   - source blobs (blobs/): re-downloadable from the control plane.
//   - project workspaces (projects/*/*): source tree, DerivedData and result
//     cache; the most expensive to lose, so they get the remainder.
//
// The agent runs one build at a time and cleanup runs after the build has
// released its workspace, so no tier is in use while it is evicted.

const (
	defaultCompilationCacheShare = 4  // 1/4 of the total budget
	defaultBlobCacheShare        = 10 // 1/10 of the total budget
	gib                          = int64(1024 * 1024 * 1024)
)

type cacheBudget struct {
	total            int64
	compilationCache int64
	blobs            int64
	projects         int64
}

func (a *Agent) cacheBudget() cacheBudget {
	total := a.cfg.CacheMaxGB * gib
	compilation := a.cfg.CompilationCacheMaxGB * gib
	if compilation <= 0 {
		compilation = total / defaultCompilationCacheShare
	}
	blobs := a.cfg.BlobCacheMaxGB * gib
	if blobs <= 0 {
		blobs = total / defaultBlobCacheShare
	}
	projects := total - compilation - blobs
	if projects < gib {
		projects = gib
	}
	return cacheBudget{total: total, compilationCache: compilation, blobs: blobs, projects: projects}
}

type cacheEntry struct {
	path string
	used time.Time
	size int64
}

// evictionReport records what one cleanup pass removed, per tier.
type evictionReport struct {
	removed []string
	freed   int64
}

func (r *evictionReport) remove(entry cacheEntry, reason string) {
	if err := removeCacheDir(entry.path); err != nil {
		log.Printf("cache cleanup: remove %s: %v", entry.path, err)
		return
	}
	r.removed = append(r.removed, filepath.Base(entry.path)+" ("+reason+")")
	r.freed += entry.size
}

func (a *Agent) cleanupCaches(currentRoot string) {
	a.cleanupMu.Lock()
	defer a.cleanupMu.Unlock()
	if time.Since(a.lastCleanup) < time.Hour {
		return
	}
	a.lastCleanup = time.Now()

	budget := a.cacheBudget()
	cutoff := time.Now().Add(-a.cfg.CacheTTL)
	tiers := []struct {
		name   string
		report evictionReport
	}{
		{name: "compilation-cache", report: a.cleanupCompilationCaches(cutoff, budget.compilationCache)},
		{name: "blobs", report: a.cleanupBlobs(cutoff, budget.blobs)},
		{name: "projects", report: a.cleanupProjects(currentRoot, cutoff, budget.projects)},
	}
	for _, tier := range tiers {
		if len(tier.report.removed) == 0 {
			continue
		}
		log.Printf("cache cleanup: %s freed %d MiB: %s", tier.name, tier.report.freed/(1024*1024), strings.Join(tier.report.removed, ", "))
	}
}

// cleanupCompilationCaches removes toolchain caches that have not been used
// within the TTL (an uninstalled or replaced Xcode), then, oldest first, whole
// toolchain caches until the tier fits. A CAS is dropped as a unit: its
// internal index cannot be trimmed safely from outside the compiler.
func (a *Agent) cleanupCompilationCaches(cutoff time.Time, maxBytes int64) evictionReport {
	var report evictionReport
	root := filepath.Join(a.cfg.CacheRoot, "toolchains")
	entries := listCacheDirs(root)
	var kept []cacheEntry
	var total int64
	for _, entry := range entries {
		if !entry.used.IsZero() && entry.used.Before(cutoff) {
			report.remove(entry, "expired")
			continue
		}
		kept = append(kept, entry)
		total += entry.size
	}
	evictOldest(kept, total, maxBytes, &report)
	return report
}

// cleanupBlobs evicts content-addressed source blobs by last use. A blob is
// touched whenever a build reads it, and any evicted blob is downloaded again
// from the control plane on demand.
func (a *Agent) cleanupBlobs(cutoff time.Time, maxBytes int64) evictionReport {
	var report evictionReport
	root := filepath.Join(a.cfg.CacheRoot, "blobs")
	var blobs []cacheEntry
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		item := cacheEntry{path: path, used: info.ModTime(), size: info.Size()}
		if item.used.Before(cutoff) {
			if os.Remove(path) == nil {
				report.freed += item.size
			}
			return nil
		}
		blobs = append(blobs, item)
		total += item.size
		return nil
	})
	if total > maxBytes {
		sort.Slice(blobs, func(i, j int) bool { return blobs[i].used.Before(blobs[j].used) })
		for _, item := range blobs {
			if total <= maxBytes {
				break
			}
			if os.Remove(item.path) == nil {
				total -= item.size
				report.freed += item.size
			}
		}
	}
	if report.freed > 0 {
		report.removed = append(report.removed, "least recently used blobs")
	}
	return report
}

// cleanupProjects evicts per-project, per-toolchain workspaces. The workspace
// the current build used is never evicted.
func (a *Agent) cleanupProjects(currentRoot string, cutoff time.Time, maxBytes int64) evictionReport {
	var report evictionReport
	projectsRoot := filepath.Join(a.cfg.CacheRoot, "projects")
	projects, _ := os.ReadDir(projectsRoot)
	var kept []cacheEntry
	var total int64
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		for _, entry := range listCacheDirs(filepath.Join(projectsRoot, project.Name())) {
			if entry.path == currentRoot {
				total += entry.size
				continue
			}
			if !entry.used.IsZero() && entry.used.Before(cutoff) {
				report.remove(entry, "expired")
				continue
			}
			kept = append(kept, entry)
			total += entry.size
		}
	}
	evictOldest(kept, total, maxBytes, &report)
	return report
}

// listCacheDirs lists the immediate subdirectories of root with their size and
// the time recorded in their last-used marker (zero when there is none).
func listCacheDirs(root string) []cacheEntry {
	children, _ := os.ReadDir(root)
	var entries []cacheEntry
	for _, child := range children {
		if !child.IsDir() {
			continue
		}
		path := filepath.Join(root, child.Name())
		used := time.Time{}
		if info, err := os.Stat(filepath.Join(path, "last-used")); err == nil {
			used = info.ModTime()
		}
		entries = append(entries, cacheEntry{path: path, used: used, size: directorySize(path)})
	}
	return entries
}

func evictOldest(entries []cacheEntry, total, maxBytes int64, report *evictionReport) {
	if total <= maxBytes {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	for _, entry := range entries {
		if total <= maxBytes {
			return
		}
		before := report.freed
		report.remove(entry, "over budget")
		if report.freed > before {
			total -= entry.size
		}
	}
}

func touchLastUsed(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "last-used"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// removeCacheDir deletes a cache directory even when tools inside it (Go
// module caches, some CocoaPods artifacts) left read-only directories.
func removeCacheDir(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(child string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(child, 0o750)
		}
		return nil
	})
	return os.RemoveAll(path)
}
