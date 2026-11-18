package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

type dependencyCacheState struct {
	Fingerprint string    `json:"fingerprint"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func dependencyFingerprint(root string, relativePaths []string, tool string) (string, error) {
	paths := append([]string(nil), relativePaths...)
	sort.Strings(paths)
	hasher := sha256.New()
	_, _ = fmt.Fprintf(hasher, "tool=%s\n", tool)
	for _, relative := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hasher, "path=%s\nsize=%d\n", relative, len(data))
		_, _ = hasher.Write(data)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func toolVersion(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return name + ":missing"
	}
	output, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return path + ":unknown"
	}
	return path + ":" + string(output)
}

func dependencyCacheHit(statePath, fingerprint string, requiredPaths ...string) bool {
	for _, path := range requiredPaths {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return false
	}
	var state dependencyCacheState
	return json.Unmarshal(data, &state) == nil && state.Fingerprint == fingerprint
}

func filesEqual(first, second string) bool {
	firstData, firstErr := os.ReadFile(first)
	secondData, secondErr := os.ReadFile(second)
	return firstErr == nil && secondErr == nil && bytes.Equal(firstData, secondData)
}

func storeDependencyCacheState(statePath, fingerprint string) error {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o750); err != nil {
		return err
	}
	data, _ := json.Marshal(dependencyCacheState{Fingerprint: fingerprint, UpdatedAt: time.Now().UTC()})
	temporary, err := os.CreateTemp(filepath.Dir(statePath), ".state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, statePath)
}
