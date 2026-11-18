package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxConcurrentBlobFetches = 8

type cacheWorkspace struct {
	Source         string
	DerivedData    string
	ManifestDigest string
	ManifestFiles  []ManifestFile
	root           string
	lock           *os.File
}

type pendingMaterialization struct {
	file   ManifestFile
	target string
}

type blobFetchResult struct {
	hash    string
	path    string
	fetched int64
	err     error
}

func (w *cacheWorkspace) close() {
	if w == nil || w.lock == nil {
		return
	}
	_ = syscall.Flock(int(w.lock.Fd()), syscall.LOCK_UN)
	_ = w.lock.Close()
	_ = os.WriteFile(filepath.Join(w.root, "last-used"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

func (a *Agent) materializeCAS(ctx context.Context, build Build, manifestPath string, developerDir string) (*cacheWorkspace, SyncMetrics, error) {
	started := time.Now()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, SyncMetrics{}, err
	}
	var manifest ProjectManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, SyncMetrics{}, fmt.Errorf("decode source manifest: %w", err)
	}
	if manifest.CacheKey == "" || manifest.CacheKey != build.CacheKey || len(manifest.Files) == 0 {
		return nil, SyncMetrics{}, fmt.Errorf("invalid or mismatched source manifest")
	}
	projectID := digestString(manifest.CacheKey)
	toolchain := a.toolchainID(developerDir)
	projectRoot := filepath.Join(a.cfg.CacheRoot, "projects", projectID)
	root := filepath.Join(projectRoot, toolchain)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, SyncMetrics{}, err
	}
	lock, err := os.OpenFile(filepath.Join(projectRoot, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, SyncMetrics{}, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		lock.Close()
		return nil, SyncMetrics{}, err
	}
	workspace := &cacheWorkspace{
		Source:         filepath.Join(root, "source"),
		DerivedData:    filepath.Join(root, "DerivedData"),
		ManifestDigest: digestManifest(manifest),
		ManifestFiles:  append([]ManifestFile(nil), manifest.Files...),
		root:           root,
		lock:           lock,
	}
	if err := os.MkdirAll(workspace.Source, 0o750); err != nil {
		workspace.close()
		return nil, SyncMetrics{}, err
	}
	if err := os.MkdirAll(workspace.DerivedData, 0o750); err != nil {
		workspace.close()
		return nil, SyncMetrics{}, err
	}

	statePath := filepath.Join(root, "manifest.json")
	previous := ProjectManifest{}
	if previousData, readErr := os.ReadFile(statePath); readErr == nil {
		_ = json.Unmarshal(previousData, &previous)
	}
	previousByPath := make(map[string]ManifestFile, len(previous.Files))
	for _, file := range previous.Files {
		previousByPath[file.Path] = file
	}
	currentPaths := make(map[string]struct{}, len(manifest.Files))
	metrics := SyncMetrics{FilesTotal: len(manifest.Files), CacheHit: len(previous.Files) > 0}
	pending := make([]pendingMaterialization, 0)
	hashes := make([]string, 0)
	for _, file := range manifest.Files {
		if !safeManifestPath(file.Path) {
			workspace.close()
			return nil, metrics, fmt.Errorf("unsafe manifest path %q", file.Path)
		}
		if file.Link != "" {
			if !safeManifestPath(file.Link) {
				workspace.close()
				return nil, metrics, fmt.Errorf("unsafe symlink target %q for %q", file.Link, file.Path)
			}
		} else if len(file.Hash) != 64 {
			workspace.close()
			return nil, metrics, fmt.Errorf("invalid manifest hash for %q", file.Path)
		}
		currentPaths[file.Path] = struct{}{}
		target := filepath.Join(workspace.Source, filepath.FromSlash(file.Path))
		prior, unchanged := previousByPath[file.Path]
		if unchanged && prior.Hash == file.Hash && prior.Link == file.Link {
			if file.Link != "" {
				if link, linkErr := os.Readlink(target); linkErr == nil && link == file.Link {
					metrics.FilesReused++
					continue
				}
			} else if info, statErr := os.Stat(target); statErr == nil && info.Mode().IsRegular() && info.Size() == file.Size {
				metrics.FilesReused++
				continue
			}
		}
		pending = append(pending, pendingMaterialization{file: file, target: target})
		if file.Link == "" {
			hashes = append(hashes, file.Hash)
		}
	}
	blobs, fetched, err := a.prefetchBlobs(ctx, hashes)
	if err != nil {
		workspace.close()
		return nil, metrics, err
	}
	metrics.BlobBytesFetched = fetched
	for _, item := range pending {
		if item.file.Link != "" {
			err = materializeSymlink(item.target, item.file.Link)
		} else {
			err = copyAtomic(blobs[item.file.Hash], item.target, os.FileMode(item.file.Mode))
		}
		if err != nil {
			workspace.close()
			return nil, metrics, err
		}
		metrics.FilesChanged++
	}
	for _, file := range previous.Files {
		if _, exists := currentPaths[file.Path]; exists || !safeManifestPath(file.Path) {
			continue
		}
		if err := os.Remove(filepath.Join(workspace.Source, filepath.FromSlash(file.Path))); err == nil || os.IsNotExist(err) {
			metrics.FilesRemoved++
		}
	}
	stateData, _ := json.Marshal(manifest)
	if err := os.WriteFile(statePath, stateData, 0o600); err != nil {
		workspace.close()
		return nil, metrics, err
	}
	_ = os.WriteFile(filepath.Join(projectRoot, "cache-key.txt"), []byte(manifest.CacheKey), 0o600)
	metrics.SourceSyncMS = time.Since(started).Milliseconds()
	return workspace, metrics, nil
}

func (a *Agent) prefetchBlobs(ctx context.Context, requested []string) (map[string]string, int64, error) {
	unique := make(map[string]struct{}, len(requested))
	for _, hash := range requested {
		unique[hash] = struct{}{}
	}
	hashes := make([]string, 0, len(unique))
	for hash := range unique {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	if len(hashes) == 0 {
		return map[string]string{}, 0, nil
	}

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string, len(hashes))
	results := make(chan blobFetchResult, len(hashes))
	for _, hash := range hashes {
		jobs <- hash
	}
	close(jobs)

	workers := min(maxConcurrentBlobFetches, len(hashes))
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for hash := range jobs {
				path, fetched, err := a.localBlob(fetchCtx, hash)
				results <- blobFetchResult{hash: hash, path: path, fetched: fetched, err: err}
				if err != nil {
					cancel()
				}
			}
		}()
	}
	go func() {
		group.Wait()
		close(results)
	}()

	paths := make(map[string]string, len(hashes))
	var fetchedBytes int64
	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		paths[result.hash] = result.path
		fetchedBytes += result.fetched
	}
	if firstErr != nil {
		return nil, fetchedBytes, firstErr
	}
	return paths, fetchedBytes, nil
}

func digestManifest(manifest ProjectManifest) string {
	canonical := ProjectManifest{
		Version:  manifest.Version,
		CacheKey: manifest.CacheKey,
		Files:    append([]ManifestFile(nil), manifest.Files...),
	}
	sort.Slice(canonical.Files, func(i, j int) bool { return canonical.Files[i].Path < canonical.Files[j].Path })
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (a *Agent) localBlob(ctx context.Context, hash string) (string, int64, error) {
	if len(hash) != 64 {
		return "", 0, fmt.Errorf("invalid blob hash")
	}
	path := filepath.Join(a.cfg.CacheRoot, "blobs", hash[:2], hash)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return path, 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", 0, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return "", 0, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.ServerURL+"/v1/agent/blobs/"+hash, nil)
	if err != nil {
		temporary.Close()
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.AgentToken)
	resp, err := a.client.Do(req)
	if err != nil {
		temporary.Close()
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		temporary.Close()
		return "", 0, fmt.Errorf("blob %s download returned %s", hash, resp.Status)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), resp.Body)
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil {
		return "", 0, fmt.Errorf("download blob %s", hash)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != hash {
		return "", 0, fmt.Errorf("downloaded blob %s failed hash verification", hash)
	}
	if err := os.Chmod(temporaryPath, 0o640); err != nil {
		return "", 0, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return "", 0, err
		}
	}
	return path, written, nil
}

func materializeSymlink(destination, target string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, destination)
}

func copyAtomic(source, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".sync-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, in); err != nil {
		temporary.Close()
		return err
	}
	if mode&0o111 != 0 {
		mode = 0o750
	} else {
		mode = 0o640
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

func safeManifestPath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || strings.HasPrefix(path, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

// toolchainID fingerprints the Xcode selected for a build. An empty
// developerDir means the system default; non-empty values are cached separately
// so concurrent builds with different Xcodes never share caches.
func (a *Agent) toolchainID(developerDir string) string {
	a.toolchainMu.Lock()
	defer a.toolchainMu.Unlock()
	if cached, ok := a.toolchains[developerDir]; ok {
		return cached
	}
	env := []string{}
	if developerDir != "" {
		env = append(env, "DEVELOPER_DIR="+developerDir)
	}
	versionCommand := exec.Command("xcodebuild", "-version")
	versionCommand.Env = childEnvironment(env)
	xcode, err := versionCommand.CombinedOutput()
	if err != nil {
		return "xcode-unknown"
	}
	sdkCommand := exec.Command("xcrun", "--sdk", "macosx", "--show-sdk-version")
	sdkCommand.Env = childEnvironment(env)
	sdk, _ := sdkCommand.CombinedOutput()
	id := digestString(string(xcode) + "\nmacos-sdk=" + strings.TrimSpace(string(sdk)))
	if a.toolchains == nil {
		a.toolchains = map[string]string{}
	}
	a.toolchains[developerDir] = id
	return id
}

func (a *Agent) compilationCachePath(developerDir string) string {
	path := filepath.Join(a.cfg.CacheRoot, "toolchains", a.toolchainID(developerDir), "CompilationCache.noindex")
	_ = os.MkdirAll(path, 0o750)
	return path
}

func (a *Agent) cleanupCaches(currentRoot string) {
	a.cleanupMu.Lock()
	defer a.cleanupMu.Unlock()
	if time.Since(a.lastCleanup) < time.Hour {
		return
	}
	a.lastCleanup = time.Now()

	cutoff := time.Now().Add(-a.cfg.CacheTTL)
	projectsRoot := filepath.Join(a.cfg.CacheRoot, "projects")
	entries, _ := os.ReadDir(projectsRoot)
	type candidate struct {
		path string
		used time.Time
		size int64
	}
	var candidates []candidate
	var total int64
	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		projectPath := filepath.Join(projectsRoot, project.Name())
		toolchains, _ := os.ReadDir(projectPath)
		for _, toolchain := range toolchains {
			if !toolchain.IsDir() {
				continue
			}
			path := filepath.Join(projectPath, toolchain.Name())
			if path == currentRoot {
				continue
			}
			info, _ := os.Stat(filepath.Join(path, "last-used"))
			used := time.Time{}
			if info != nil {
				used = info.ModTime()
			}
			size := directorySize(path)
			total += size
			if !used.IsZero() && used.Before(cutoff) {
				_ = os.RemoveAll(path)
				total -= size
				continue
			}
			candidates = append(candidates, candidate{path: path, used: used, size: size})
		}
	}
	maxBytes := a.cfg.CacheMaxGB * 1024 * 1024 * 1024
	if total <= maxBytes {
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used.Before(candidates[j].used) })
	for _, item := range candidates {
		if total <= maxBytes {
			break
		}
		_ = os.RemoveAll(item.path)
		total -= item.size
	}
}

func directorySize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
