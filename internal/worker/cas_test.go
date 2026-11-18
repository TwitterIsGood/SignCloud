package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaterializeSymlinkResolvesAndReplaces(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "Versions", "Current", "Lib")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("binary"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "Lib")
	if err := os.WriteFile(link, []byte("stale"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := materializeSymlink(link, "Versions/Current/Lib"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(link)
	if err != nil || string(data) != "binary" {
		t.Fatalf("symlink read = %q, %v; want binary", data, err)
	}
	if resolved, err := os.Readlink(link); err != nil || resolved != "Versions/Current/Lib" {
		t.Fatalf("readlink = %q, %v", resolved, err)
	}
	if err := materializeSymlink(link, "Versions/Current/Lib"); err != nil {
		t.Fatalf("replacing an existing symlink failed: %v", err)
	}
}

func TestPrefetchBlobsUsesBoundedConcurrencyAndDeduplicates(t *testing.T) {
	contents := make(map[string][]byte)
	var hashes []string
	for index := range 6 {
		content := []byte(fmt.Sprintf("blob-%d", index))
		sum := sha256.Sum256(content)
		hash := hex.EncodeToString(sum[:])
		contents[hash] = content
		hashes = append(hashes, hash)
	}
	hashes = append(hashes, hashes[0], hashes[1])

	var active atomic.Int32
	var maximum atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		time.Sleep(25 * time.Millisecond)
		hash := filepath.Base(request.URL.Path)
		content, ok := contents[hash]
		if !ok {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write(content)
	}))
	defer server.Close()

	agent := New(Config{ServerURL: server.URL, AgentToken: "test", CacheRoot: t.TempDir()})
	paths, fetched, err := agent.prefetchBlobs(context.Background(), hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(contents) {
		t.Fatalf("prefetched %d unique blobs, want %d", len(paths), len(contents))
	}
	if requests.Load() != int32(len(contents)) {
		t.Fatalf("made %d requests, want %d", requests.Load(), len(contents))
	}
	if maximum.Load() < 2 || maximum.Load() > maxConcurrentBlobFetches {
		t.Fatalf("maximum concurrency = %d, want 2..%d", maximum.Load(), maxConcurrentBlobFetches)
	}
	var expectedBytes int64
	for _, content := range contents {
		expectedBytes += int64(len(content))
	}
	if fetched != expectedBytes {
		t.Fatalf("fetched %d bytes, want %d", fetched, expectedBytes)
	}
}
