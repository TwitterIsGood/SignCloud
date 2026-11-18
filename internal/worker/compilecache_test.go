package worker

import (
	"strings"
	"testing"
)

func TestCountCompilationCacheResults(t *testing.T) {
	output := []byte("SwiftCompile normal arm64 Foo.swift\nCache hit\n\nCache miss\nCompileC x.o\n  Cache hit  \nnot a Cache hit line\n")
	hits, misses := countCompilationCacheResults(output)
	if hits != 2 || misses != 1 {
		t.Fatalf("hits=%d misses=%d", hits, misses)
	}
}

func TestCompilationCacheFailureOnlyMatchesCacheErrors(t *testing.T) {
	if _, ok := compilationCacheFailure([]byte("Foo.swift:3:1: error: cannot find 'bar' in scope\n** BUILD FAILED **")); ok {
		t.Fatal("source error must not be treated as a cache failure")
	}
	if _, ok := compilationCacheFailure([]byte("note: Cache miss while compiling\n")); ok {
		t.Fatal("cache output without an error must not trigger a retry")
	}
	marker, ok := compilationCacheFailure([]byte("error: failed to open llvm-cas at /tmp/cas: corrupt\n"))
	if !ok || marker != "llvm-cas" {
		t.Fatalf("expected llvm-cas failure, got %q %v", marker, ok)
	}
}

func TestCompilationCacheArgsEnableCaching(t *testing.T) {
	args := strings.Join(compilationCacheArgs("/cache/cas"), " ")
	for _, want := range []string{"COMPILATION_CACHE_ENABLE_CACHING=YES", "COMPILATION_CACHE_CAS_PATH=/cache/cas"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %s in %s", want, args)
		}
	}
}
