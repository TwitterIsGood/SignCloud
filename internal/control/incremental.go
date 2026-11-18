package control

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type ManifestFile struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode,omitempty"`
	// Link marks a symbolic link; it carries the relative target instead of a blob.
	Link string `json:"link,omitempty"`
}

type ProjectManifest struct {
	Version  int            `json:"version"`
	CacheKey string         `json:"cache_key"`
	Files    []ManifestFile `json:"files"`
}

type IncrementalBuildRequest struct {
	Name     string          `json:"name"`
	Kind     string          `json:"kind"`
	CacheKey string          `json:"cache_key"`
	Spec     json.RawMessage `json:"spec"`
	Files    []ManifestFile  `json:"files"`
}

func (s *Server) planUpload(w http.ResponseWriter, r *http.Request) {
	var manifest ProjectManifest
	if err := decodeLimitedJSON(w, r, &manifest, 64<<20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := validateManifest(manifest.CacheKey, manifest.Files); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	missing := make([]string, 0)
	var totalBytes, missingBytes int64
	seenHashes := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		if file.Link != "" {
			continue
		}
		totalBytes += file.Size
		if _, seen := seenHashes[file.Hash]; seen {
			continue
		}
		seenHashes[file.Hash] = struct{}{}
		if !regularFileExists(s.blobPath(file.Hash)) {
			missing = append(missing, file.Hash)
			missingBytes += file.Size
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files":         len(manifest.Files),
		"total_bytes":   totalBytes,
		"missing":       missing,
		"missing_files": len(missing),
		"missing_bytes": missingBytes,
		"reused_bytes":  totalBytes - missingBytes,
	})
}

func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chiParam(r, "hash"))
	if !sha256Pattern.MatchString(hash) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid SHA-256 hash"})
		return
	}
	destination := s.blobPath(hash)
	if regularFileExists(destination) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".upload-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	maxBytes := s.cfg.MaxUploadMB * 1024 * 1024
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hasher), io.LimitReader(r.Body, maxBytes+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil {
		http.Error(w, "blob upload failed", http.StatusBadRequest)
		return
	}
	if written > maxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "blob exceeds upload limit"})
		return
	}
	if hex.EncodeToString(hasher.Sum(nil)) != hash {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "blob hash mismatch"})
		return
	}
	if err := os.Chmod(temporaryName, 0o640); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(temporaryName, destination); err != nil && !regularFileExists(destination) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) putBlobBatch(w http.ResponseWriter, r *http.Request) {
	maxBytes := s.cfg.MaxUploadMB * 1024 * 1024
	temporary, err := os.CreateTemp(filepath.Join(s.cfg.StorageDir, "tmp"), "blob-batch-*.zip")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, copyErr := io.Copy(temporary, io.LimitReader(r.Body, maxBytes+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "batch upload failed"})
		return
	}
	if written > maxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "batch exceeds upload limit"})
		return
	}
	archive, err := zip.OpenReader(temporaryPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid blob batch zip"})
		return
	}
	defer archive.Close()
	uploaded, reused := 0, 0
	for _, entry := range archive.File {
		hash := strings.ToLower(entry.Name)
		if entry.FileInfo().IsDir() || !sha256Pattern.MatchString(hash) || entry.UncompressedSize64 > uint64(maxBytes) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch entry " + entry.Name})
			return
		}
		destination := s.blobPath(hash)
		if regularFileExists(destination) {
			reused++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		source, err := entry.Open()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		blobTemp, err := os.CreateTemp(filepath.Dir(destination), ".batch-*")
		if err != nil {
			source.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		blobTempPath := blobTemp.Name()
		hasher := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(blobTemp, hasher), source)
		source.Close()
		closeErr := blobTemp.Close()
		if copyErr != nil || closeErr != nil || hex.EncodeToString(hasher.Sum(nil)) != hash {
			_ = os.Remove(blobTempPath)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "batch entry hash mismatch " + hash})
			return
		}
		_ = os.Chmod(blobTempPath, 0o640)
		if err := os.Rename(blobTempPath, destination); err != nil && !regularFileExists(destination) {
			_ = os.Remove(blobTempPath)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		uploaded++
	}
	writeJSON(w, http.StatusCreated, map[string]any{"uploaded": uploaded, "reused": reused, "compressed_bytes": written})
}

func (s *Server) createIncrementalBuild(w http.ResponseWriter, r *http.Request) {
	var request IncrementalBuildRequest
	if err := decodeLimitedJSON(w, r, &request, 64<<20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Kind = strings.TrimSpace(request.Kind)
	if request.Name == "" || !validKind(request.Kind) || request.Kind == "resign" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incremental builds require name and kind xcode, expo, simulator, macos, resolve, or verify"})
		return
	}
	if err := validateManifest(request.CacheKey, request.Files); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(request.Spec) == 0 {
		request.Spec = json.RawMessage(`{}`)
	}
	if _, err := parseAndValidateBuildSpec(request.Kind, request.Spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var totalBytes int64
	for _, file := range request.Files {
		if file.Link != "" {
			continue
		}
		totalBytes += file.Size
		if !regularFileExists(s.blobPath(file.Hash)) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "missing blob " + file.Hash})
			return
		}
	}
	id := randomID(16)
	token := randomID(24)
	manifest := ProjectManifest{Version: 1, CacheKey: request.CacheKey, Files: request.Files}
	manifestData, _ := json.Marshal(manifest)
	sourceName := "manifest.json"
	sourcePath := filepath.Join(s.cfg.StorageDir, "sources", id+"-"+sourceName)
	if err := os.WriteFile(sourcePath, manifestData, 0o640); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	metrics, _ := json.Marshal(map[string]any{"manifest_files": len(request.Files), "manifest_bytes": totalBytes})
	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO builds (id, name, kind, status, source_name, source_path, install_token, spec, cache_key, source_mode, metrics)
		VALUES ($1, $2, $3, 'queued', $4, $5, $6, $7, $8, 'cas', $9)`,
		id, request.Name, request.Kind, sourceName, sourcePath, token, request.Spec, request.CacheKey, metrics)
	if err != nil {
		_ = os.Remove(sourcePath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create incremental build failed"})
		return
	}
	build, err := s.fetchBuild(r.Context(), "WHERE id=$1", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.decorate(&build, true)
	writeJSON(w, http.StatusCreated, build)
}

func (s *Server) downloadBlob(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chiParam(r, "hash"))
	if !sha256Pattern.MatchString(hash) {
		http.NotFound(w, r)
		return
	}
	path := s.blobPath(hash)
	if !regularFileExists(path) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+hash+`"`)
	http.ServeFile(w, r, path)
}

func (s *Server) blobPath(hash string) string {
	return filepath.Join(s.cfg.StorageDir, "blobs", hash[:2], hash)
}

func validateManifest(cacheKey string, files []ManifestFile) error {
	if strings.TrimSpace(cacheKey) == "" || len(cacheKey) > 200 {
		return errors.New("cache_key is required and must be at most 200 characters")
	}
	if len(files) == 0 || len(files) > 200000 {
		return errors.New("manifest must contain 1 to 200000 files")
	}
	seenPaths := make(map[string]struct{}, len(files))
	for _, file := range files {
		if !safeRelativePath(file.Path) {
			return fmt.Errorf("invalid manifest entry %q", file.Path)
		}
		if _, exists := seenPaths[file.Path]; exists {
			return fmt.Errorf("duplicate manifest path %q", file.Path)
		}
		seenPaths[file.Path] = struct{}{}
		if file.Link != "" {
			if !safeRelativePath(file.Link) {
				return fmt.Errorf("invalid symlink target %q for %q", file.Link, file.Path)
			}
			continue
		}
		if !sha256Pattern.MatchString(file.Hash) || file.Size < 0 {
			return fmt.Errorf("invalid manifest entry %q", file.Path)
		}
	}
	return nil
}

func safeRelativePath(path string) bool {
	if path == "" || strings.ContainsRune(path, '\x00') || strings.Contains(path, "\\") || strings.HasPrefix(path, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	return clean == path && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, target any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func chiParam(r *http.Request, name string) string {
	return chi.URLParam(r, name)
}
