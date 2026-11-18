package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const resultCacheVersion = 5

type cachedBuildResult struct {
	Fingerprint  string           `json:"fingerprint"`
	ArtifactName string           `json:"artifact_name"`
	Metadata     ArtifactMetadata `json:"metadata"`
	CreatedAt    time.Time        `json:"created_at"`
}

func (a *Agent) resultFingerprint(workspace *cacheWorkspace, build Build, spec BuildSpec, developerDir string) (string, error) {
	signingDigest := "unsigned"
	if build.Kind != "simulator" {
		var err error
		signingDigest, err = a.signingStateDigest(build.Kind, spec, developerDir)
		if err != nil {
			return "", err
		}
	}
	cacheName := ""
	if build.Kind == "macos" {
		cacheName = build.Name
	}
	input := struct {
		Version        int       `json:"version"`
		ManifestDigest string    `json:"manifest_digest"`
		Kind           string    `json:"kind"`
		Name           string    `json:"name,omitempty"`
		Spec           BuildSpec `json:"spec"`
		Toolchain      string    `json:"toolchain"`
		SigningDigest  string    `json:"signing_digest"`
	}{
		Version:        resultCacheVersion,
		ManifestDigest: workspace.ManifestDigest,
		Kind:           build.Kind,
		Name:           cacheName,
		Spec:           spec,
		Toolchain:      a.toolchainID(developerDir),
		SigningDigest:  signingDigest,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func resultCacheEligible(workspace *cacheWorkspace, kind string) (bool, string) {
	if kind == "resolve" {
		return false, "package resolution always runs against the current package graph"
	}
	if kind == "verify" {
		return false, "verification must run on a real device every time"
	}
	inputPaths := make(map[string]struct{}, len(workspace.ManifestFiles))
	for _, file := range workspace.ManifestFiles {
		inputPaths[file.Path] = struct{}{}
	}
	if kind == "expo" {
		locked := false
		for _, name := range []string{"pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb", "package-lock.json"} {
			if _, exists := inputPaths[name]; exists {
				locked = true
				break
			}
		}
		if !locked {
			return false, "JavaScript dependencies are not locked"
		}
	}
	remoteSwiftPackages := false
	packageResolved := false
	unlockedPods := false
	for _, file := range workspace.ManifestFiles {
		if file.Link != "" {
			continue
		}
		switch filepath.Base(file.Path) {
		case "Package.resolved":
			packageResolved = true
		case "Podfile":
			lockPath := filepath.ToSlash(filepath.Join(filepath.Dir(file.Path), "Podfile.lock"))
			if _, exists := inputPaths[lockPath]; !exists {
				unlockedPods = true
			}
		case "project.pbxproj":
			data, err := os.ReadFile(filepath.Join(workspace.Source, filepath.FromSlash(file.Path)))
			if err == nil && strings.Contains(string(data), "XCRemoteSwiftPackageReference") {
				remoteSwiftPackages = true
			}
		}
	}
	if unlockedPods {
		return false, "CocoaPods dependencies are not locked"
	}
	if remoteSwiftPackages && !packageResolved {
		return false, "Swift package dependencies are not locked"
	}
	return true, ""
}

func (a *Agent) signingStateDigest(kind string, spec BuildSpec, developerDir string) (string, error) {
	if kind == "macos" && (spec.Signing == "adhoc" || (spec.Signing == "" && !spec.Notarize)) {
		return digestString("macos-adhoc:" + a.toolchainID(developerDir)), nil
	}
	xcodeEnv, err := a.xcodeEnvironment()
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	identity := a.cfg.SigningIdentity
	configVersion := ""
	if spec.Signing == "developer-id" {
		identity = a.cfg.MacOSDeveloperIDIdentity
		configVersion = a.cfg.MacOSSigningConfigVersion
	}
	_, _ = fmt.Fprintf(hasher, "team=%s\nagent_team=%s\nidentity=%s\nmethod=%s\nconfig_version=%s\nnotary_profile_configured=%t\n",
		spec.TeamID, a.cfg.TeamID, identity, spec.ExportMethod, configVersion, a.cfg.MacOSNotaryProfile != "")
	command := exec.Command("security", "find-identity", "-v", "-p", "codesigning")
	if spec.Signing == "developer-id" && a.cfg.MacOSKeychainPath != "" {
		command.Args = append(command.Args, a.cfg.MacOSKeychainPath)
	}
	command.Env = childEnvironment(xcodeEnv)
	identities, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read signing identities: %w", err)
	}
	_, _ = hasher.Write(identities)

	var profiles []string
	for _, root := range []string{
		filepath.Join(a.buildHome, "Library", "Developer", "Xcode", "UserData", "Provisioning Profiles"),
		filepath.Join(a.buildHome, "Library", "MobileDevice", "Provisioning Profiles"),
	} {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr == nil && !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".mobileprovision") {
				profiles = append(profiles, path)
			}
			return nil
		})
	}
	sort.Strings(profiles)
	for _, profile := range profiles {
		data, readErr := os.ReadFile(profile)
		if readErr != nil {
			return "", readErr
		}
		_, _ = hasher.Write([]byte(filepath.Base(profile)))
		_, _ = hasher.Write(data)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func cachedResult(workspace *cacheWorkspace, fingerprint string) (string, ArtifactMetadata, bool) {
	root := filepath.Join(workspace.root, "result-cache")
	data, err := os.ReadFile(filepath.Join(root, "metadata.json"))
	if err != nil {
		return "", ArtifactMetadata{}, false
	}
	var cached cachedBuildResult
	if json.Unmarshal(data, &cached) != nil || cached.Fingerprint != fingerprint || cached.ArtifactName == "" {
		return "", ArtifactMetadata{}, false
	}
	artifact := filepath.Join(root, cached.ArtifactName)
	info, err := os.Stat(artifact)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", ArtifactMetadata{}, false
	}
	if cached.Metadata.AppZip != "" {
		appZip := filepath.Join(root, filepath.Base(cached.Metadata.AppZip))
		if info, appZipErr := os.Stat(appZip); appZipErr != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return "", ArtifactMetadata{}, false
		}
		cached.Metadata.AppZip = appZip
	}
	return artifact, cached.Metadata, true
}

func storeCachedResult(workspace *cacheWorkspace, fingerprint, artifact string, metadata ArtifactMetadata) error {
	temporary, err := os.MkdirTemp(workspace.root, ".result-cache-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	artifactName := filepath.Base(artifact)
	target := filepath.Join(temporary, artifactName)
	if err := os.Link(artifact, target); err != nil {
		if err := copyFile(artifact, target); err != nil {
			return err
		}
	}
	if metadata.AppZip != "" {
		appZipName := filepath.Base(metadata.AppZip)
		appZipTarget := filepath.Join(temporary, appZipName)
		if err := os.Link(metadata.AppZip, appZipTarget); err != nil {
			if err := copyFile(metadata.AppZip, appZipTarget); err != nil {
				return err
			}
		}
		metadata.AppZip = appZipName
	}
	cached := cachedBuildResult{
		Fingerprint:  fingerprint,
		ArtifactName: artifactName,
		Metadata:     metadata,
		CreatedAt:    time.Now().UTC(),
	}
	data, _ := json.Marshal(cached)
	if err := os.WriteFile(filepath.Join(temporary, "metadata.json"), data, 0o600); err != nil {
		return err
	}
	current := filepath.Join(workspace.root, "result-cache")
	if err := os.RemoveAll(current); err != nil {
		return err
	}
	if err := os.Rename(temporary, current); err != nil {
		return err
	}
	return nil
}
