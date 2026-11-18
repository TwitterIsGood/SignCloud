package worker

import (
	"bufio"
	"bytes"
	"context"
	"strings"
)

// Xcode compilation caching (Xcode 26+) is off unless
// COMPILATION_CACHE_ENABLE_CACHING=YES is set; COMPILATION_CACHE_CAS_PATH on
// its own only names a directory that is never written. The worker enables
// caching for every compile through xcodebuildCompile and keeps one CAS per
// toolchain, so a cold DerivedData, a Debug/Archive switch or a reverted edit
// reuses earlier compiler outputs.

const (
	compilationCacheAuto = "auto"
	compilationCacheOff  = "off"
)

// compilationCacheFailureMarkers identify xcodebuild failures caused by the
// compilation cache itself rather than by the caller's source. Only these
// trigger a retry without caching, so genuine compile errors are not built
// twice.
var compilationCacheFailureMarkers = []string{
	"compilation cache",
	"compilation caching",
	"llvm-cas",
	"cas database",
	"ondisk cas",
	"cas plugin",
	"cache key",
	"cache-compile-job",
	"failed to load cache",
	"caching is not supported",
}

func (a *Agent) compilationCacheEnabled() bool {
	return a.cfg.CompilationCache != compilationCacheOff
}

func compilationCacheArgs(casPath string) []string {
	return []string{
		"COMPILATION_CACHE_ENABLE_CACHING=YES",
		"COMPILATION_CACHE_CAS_PATH=" + casPath,
		"COMPILATION_CACHE_KEEP_CAS_DIRECTORY=YES",
	}
}

// xcodebuildCompile runs a compiling xcodebuild invocation with compilation
// caching enabled when allowed. Build settings are order-independent on the
// xcodebuild command line, so they are appended after the action. A failure
// that names the cache is retried once without caching and the reason is
// recorded; every other failure is returned unchanged.
func (r *jobRunner) xcodebuildCompile(ctx context.Context, root string, env []string, args []string) error {
	if !r.agent.compilationCacheEnabled() {
		r.compilationCacheReason = "disabled by SIGNCLOUD_COMPILATION_CACHE=off"
		_, err := r.commandOutput(ctx, root, env, "xcodebuild", args...)
		return err
	}
	cached := append(append([]string{}, args...), compilationCacheArgs(r.agent.compilationCachePath(r.developerDir))...)
	output, err := r.commandOutput(ctx, root, env, "xcodebuild", cached...)
	hits, misses := countCompilationCacheResults(output)
	r.compilationCacheHits += hits
	r.compilationCacheMisses += misses
	if err == nil {
		return nil
	}
	marker, cacheFailure := compilationCacheFailure(output)
	if !cacheFailure {
		return err
	}
	r.compilationCacheReason = "retried without compilation cache after: " + marker
	r.agent.log(ctx, r.buildID, "Compilation cache failure detected ("+marker+"); retrying without compilation cache\n")
	_, retryErr := r.commandOutput(ctx, root, env, "xcodebuild", args...)
	return retryErr
}

// countCompilationCacheResults counts the per-task "Cache hit" and
// "Cache miss" lines xcodebuild prints when compilation caching is active.
func countCompilationCacheResults(output []byte) (hits, misses int) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "Cache hit":
			hits++
		case "Cache miss":
			misses++
		}
	}
	return hits, misses
}

func compilationCacheFailure(output []byte) (string, bool) {
	for _, line := range strings.Split(string(output), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "error") {
			continue
		}
		for _, marker := range compilationCacheFailureMarkers {
			if strings.Contains(lower, marker) {
				return marker, true
			}
		}
	}
	return "", false
}
