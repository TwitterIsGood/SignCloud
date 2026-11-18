package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

type jobRunner struct {
	agent          *Agent
	buildID        string
	buildName      string
	jobDir         string
	strategy       string
	fallbackReason string
	developerDir   string
	xcodeMS        int64
	archiveMS      int64
	exportMS       int64
	dependencyMS   int64
	prebuildMS     int64
	packagingMS    int64
	dependencyHit  bool
	podCacheHit    bool
	cpuUserMS      int64
	cpuSystemMS    int64
	peakRSSBytes   int64
	verification   *VerificationMetrics

	compilationCacheHits   int
	compilationCacheMisses int
	compilationCacheReason string
	buildProfile           string
	buildProfileApplied    []string
	buildProfileNotes      []string
}

type fastPackageFallbackError struct{ err error }

func (e *fastPackageFallbackError) Error() string { return e.err.Error() }
func (e *fastPackageFallbackError) Unwrap() error { return e.err }

// withXcodeVersion pins DEVELOPER_DIR for xcodebuild/xcrun so a build can use a
// specific installed Xcode instead of the system xcode-select default.
func (r *jobRunner) withXcodeVersion(env []string) []string {
	if r.developerDir == "" {
		return env
	}
	return append(append([]string{}, env...), "DEVELOPER_DIR="+r.developerDir)
}

func (r *jobRunner) applyMetrics(metrics *SyncMetrics) {
	metrics.BuildStrategy = r.strategy
	metrics.FallbackReason = r.fallbackReason
	metrics.XcodeMS = r.xcodeMS
	metrics.ArchiveMS = r.archiveMS
	metrics.ExportMS = r.exportMS
	metrics.DependencyMS = r.dependencyMS
	metrics.PrebuildMS = r.prebuildMS
	metrics.PackagingMS = r.packagingMS
	metrics.DependencyCacheHit = r.dependencyHit
	metrics.PodCacheHit = r.podCacheHit
	metrics.CPUUserMS = r.cpuUserMS
	metrics.CPUSystemMS = r.cpuSystemMS
	metrics.PeakRSSBytes = r.peakRSSBytes
	metrics.CompilationCacheHits = r.compilationCacheHits
	metrics.CompilationCacheMisses = r.compilationCacheMisses
	metrics.CompilationCacheReason = r.compilationCacheReason
	metrics.BuildProfile = r.buildProfile
	metrics.BuildProfileSettings = r.buildProfileApplied
	metrics.BuildProfileNotes = r.buildProfileNotes
	metrics.Verification = r.verification
}

func (r *jobRunner) buildXcode(ctx context.Context, source string, spec BuildSpec) (string, error) {
	root := filepath.Join(r.jobDir, "source")
	if err := unzip(source, root); err != nil {
		return "", fmt.Errorf("extract source: %w", err)
	}
	root = collapseSingleDirectory(root)
	return r.buildXcodeAt(ctx, root, spec, filepath.Join(r.jobDir, "DerivedData"))
}

func (r *jobRunner) buildXcodeAt(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, error) {
	if spec.FastPackage {
		if eligible, reason := r.fastPackageEligible(spec); eligible {
			artifact, err := r.buildDevelopmentIPA(ctx, root, spec, derivedData)
			if err == nil {
				return artifact, nil
			}
			var fallback *fastPackageFallbackError
			if !errors.As(err, &fallback) {
				return "", err
			}
			r.fallbackReason = fallback.Error()
			r.agent.log(ctx, r.buildID, "fast package fallback: "+fallback.Error()+"\n")
		} else {
			r.fallbackReason = reason
			r.agent.log(ctx, r.buildID, "fast package ineligible: "+reason+"; using Archive/Export\n")
		}
	}
	return r.archiveAndExport(ctx, root, spec, derivedData)
}

func (r *jobRunner) fastPackageEligible(spec BuildSpec) (bool, string) {
	method := spec.ExportMethod
	if method == "" {
		method = r.agent.cfg.ExportMethod
	}
	if method != "" && method != "development" && method != "debugging" {
		return false, "fast packaging supports Development exports only"
	}
	configuration := spec.Configuration
	if configuration == "" {
		configuration = "Debug"
	}
	if !strings.EqualFold(configuration, "Debug") {
		return false, "fast packaging requires a Debug configuration"
	}
	return true, ""
}

func (r *jobRunner) buildSimulator(ctx context.Context, source string, spec BuildSpec) (string, error) {
	root := filepath.Join(r.jobDir, "source")
	if err := unzip(source, root); err != nil {
		return "", fmt.Errorf("extract source: %w", err)
	}
	root = collapseSingleDirectory(root)
	return r.buildSimulatorAt(ctx, root, spec, filepath.Join(r.jobDir, "DerivedData"))
}

func (r *jobRunner) buildSimulatorAt(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, error) {
	app, err := r.buildSimulatorAppAt(ctx, root, spec, derivedData)
	if err != nil {
		return "", err
	}
	output := filepath.Join(r.jobDir, strings.TrimSuffix(filepath.Base(app), ".app")+"-simulator.zip")
	started := time.Now()
	if err := r.command(ctx, filepath.Dir(app), nil, "/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", filepath.Base(app), output); err != nil {
		return "", err
	}
	r.packagingMS += time.Since(started).Milliseconds()
	return output, nil
}

func (r *jobRunner) buildSimulatorAppAt(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, error) {
	r.strategy = "simulator"
	xcodeEnv, err := r.agent.xcodeEnvironment()
	if err != nil {
		return "", err
	}
	workspace, project, err := findXcodeContainer(root, spec)
	if err != nil {
		return "", err
	}
	containerArgs := []string{}
	if workspace != "" {
		containerArgs = []string{"-workspace", workspace}
	} else {
		containerArgs = []string{"-project", project}
	}
	scheme := spec.Scheme
	if scheme == "" {
		scheme, err = r.discoverScheme(ctx, root, containerArgs)
		if err != nil {
			return "", err
		}
	}
	configuration := spec.Configuration
	if configuration == "" {
		configuration = "Debug"
	}
	args := append([]string{}, containerArgs...)
	args = append(args, "-scheme", scheme, "-configuration", configuration)
	args = append(args, r.packageResolutionArgs(root)...)
	args = append(args, xcodeTrustArgs()...)
	args = append(args, "-parallelizeTargets",
		"-sdk", "iphonesimulator", "-destination", "generic/platform=iOS Simulator",
		"-derivedDataPath", derivedData, "CODE_SIGNING_ALLOWED=NO")
	args = r.applyBuildProfile(ctx, args, root, spec, configuration, profileSimulator)
	args = append(args, "build")
	started := time.Now()
	if err := r.xcodebuildCompile(ctx, root, r.withXcodeVersion(xcodeEnv), args); err != nil {
		return "", err
	}
	r.xcodeMS += time.Since(started).Milliseconds()
	return findSimulatorApp(filepath.Join(derivedData, "Build", "Products"))
}

func (r *jobRunner) resolvePackages(ctx context.Context, source string, spec BuildSpec) (string, error) {
	root := filepath.Join(r.jobDir, "source")
	if err := unzip(source, root); err != nil {
		return "", fmt.Errorf("extract source: %w", err)
	}
	root = collapseSingleDirectory(root)
	return r.resolvePackagesAt(ctx, root, spec)
}

func (r *jobRunner) resolvePackagesAt(ctx context.Context, root string, spec BuildSpec) (string, error) {
	r.strategy = "swiftpm-resolve"
	workspace, project, err := findXcodeContainer(root, spec)
	if err != nil {
		return "", err
	}
	containerArgs := []string{}
	if workspace != "" {
		containerArgs = []string{"-workspace", workspace}
	} else {
		containerArgs = []string{"-project", project}
	}
	xcodeEnv, err := r.agent.xcodeEnvironment()
	if err != nil {
		return "", err
	}
	// Never pass packageResolutionArgs here: resolution is the one job that must
	// be allowed to update a stale Package.resolved from the remote package graph.
	args := append(append([]string{}, containerArgs...), "-resolvePackageDependencies")
	args = append(args, xcodeTrustArgs()...)
	started := time.Now()
	if err := r.command(ctx, root, r.withXcodeVersion(xcodeEnv), "xcodebuild", args...); err != nil {
		return "", err
	}
	r.dependencyMS += time.Since(started).Milliseconds()
	files, err := packageResolvedFiles(root)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no Package.resolved was written; the project has no Swift package dependencies")
	}
	output := filepath.Join(r.jobDir, "Package.resolved.zip")
	if err := zipPackageResolved(root, files, output); err != nil {
		return "", err
	}
	return output, nil
}

func packageResolvedFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			switch entry.Name() {
			case ".git", ".build", "DerivedData", "SourcePackages", "node_modules", "Pods", "build":
				return filepath.SkipDir
			}
		}
		if !entry.IsDir() && entry.Name() == "Package.resolved" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func zipPackageResolved(root string, files []string, output string) (err error) {
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(out)
	for _, path := range files {
		relative, relativeErr := filepath.Rel(root, path)
		if relativeErr != nil {
			_ = archive.Close()
			_ = out.Close()
			return relativeErr
		}
		header := &zip.FileHeader{Name: filepath.ToSlash(relative), Method: zip.Deflate}
		header.SetMode(0o644)
		writer, createErr := archive.CreateHeader(header)
		if createErr != nil {
			_ = archive.Close()
			_ = out.Close()
			return createErr
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			_ = archive.Close()
			_ = out.Close()
			return openErr
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			_ = archive.Close()
			_ = out.Close()
			return copyErr
		}
		if closeErr != nil {
			_ = archive.Close()
			_ = out.Close()
			return closeErr
		}
	}
	if err := archive.Close(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (r *jobRunner) buildDevelopmentIPA(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, error) {
	r.strategy = "fast-development"
	xcodeEnv, err := r.agent.signingEnvironment()
	if err != nil {
		return "", err
	}
	method := spec.ExportMethod
	if method == "" {
		method = r.agent.cfg.ExportMethod
	}
	if method != "" && method != "development" && method != "debugging" {
		return "", fmt.Errorf("fast_package supports development exports only")
	}
	workspace, project, err := findXcodeContainer(root, spec)
	if err != nil {
		return "", err
	}
	containerArgs := []string{}
	if workspace != "" {
		containerArgs = []string{"-workspace", workspace}
	} else {
		containerArgs = []string{"-project", project}
	}
	scheme := spec.Scheme
	if scheme == "" {
		scheme, err = r.discoverScheme(ctx, root, containerArgs)
		if err != nil {
			return "", err
		}
	}
	configuration := spec.Configuration
	if configuration == "" {
		configuration = "Debug"
	}
	teamID := spec.TeamID
	if teamID == "" {
		teamID = r.agent.cfg.TeamID
	}
	if teamID == "" {
		return "", fmt.Errorf("team ID is required in the build spec or APPLE_TEAM_ID")
	}

	args := append([]string{}, containerArgs...)
	args = append(args, "-scheme", scheme, "-configuration", configuration)
	args = append(args, r.packageResolutionArgs(root)...)
	args = append(args, xcodeTrustArgs()...)
	args = append(args, "-parallelizeTargets", "-destination", "generic/platform=iOS",
		"-derivedDataPath", derivedData,
		"DEVELOPMENT_TEAM="+teamID)
	// Manual signing settings on the xcodebuild command line apply to every
	// target, and SwiftPM targets reject a provisioning profile, so the wildcard
	// profile is embedded after the (automatically signed) build instead.
	profile := strings.TrimSpace(r.agent.cfg.WildcardProfile)
	embedProfile := false
	if profile != "" {
		eligible, reason := wildcardSigningEligible(project)
		if eligible {
			embedProfile = true
			r.agent.log(ctx, r.buildID, "Will embed wildcard Development profile: "+profile+"\n")
		} else {
			r.agent.log(ctx, r.buildID, "Wildcard profile not applicable: "+reason+"\n")
		}
	}
	args = r.applyBuildProfile(ctx, args, root, spec, configuration, profileDevice)
	args = append(args, "build")
	if spec.AllowProvisioningUpdates {
		args = append(args, r.provisioningAuthArgs()...)
	}
	started := time.Now()
	if err := r.xcodebuildCompile(ctx, root, r.withXcodeVersion(xcodeEnv), args); err != nil {
		return "", err
	}
	r.xcodeMS += time.Since(started).Milliseconds()
	packageStarted := time.Now()
	app, err := findDeviceApp(filepath.Join(derivedData, "Build", "Products"))
	if err != nil {
		return "", &fastPackageFallbackError{err: err}
	}
	if embedProfile {
		profilePath, err := findProvisioningProfile(profile)
		if err != nil {
			return "", &fastPackageFallbackError{err: err}
		}
		if err := embedProvisioningProfile(app, profilePath); err != nil {
			return "", &fastPackageFallbackError{err: err}
		}
		if err := r.command(ctx, root, nil, "/usr/bin/codesign",
			"--force", "--sign", r.agent.cfg.SigningIdentity,
			"--preserve-metadata=identifier,entitlements,flags",
			"--generate-entitlement-der", app); err != nil {
			return "", &fastPackageFallbackError{err: fmt.Errorf("resign app with embedded profile: %w", err)}
		}
		r.agent.log(ctx, r.buildID, "Embedded provisioning profile: "+profile+"\n")
	}
	if err := r.command(ctx, root, nil, "/usr/bin/codesign", "--verify", "--deep", "--strict", app); err != nil {
		return "", &fastPackageFallbackError{err: fmt.Errorf("verify development app signature: %w", err)}
	}

	packageRoot := filepath.Join(r.jobDir, "fast-package")
	payload := filepath.Join(packageRoot, "Payload")
	if err := os.MkdirAll(payload, 0o750); err != nil {
		return "", err
	}
	packagedApp := filepath.Join(payload, filepath.Base(app))
	if err := r.command(ctx, root, nil, "/usr/bin/ditto", app, packagedApp); err != nil {
		return "", &fastPackageFallbackError{err: err}
	}
	output := filepath.Join(r.jobDir, strings.TrimSuffix(filepath.Base(app), ".app")+"-development.ipa")
	if err := r.command(ctx, packageRoot, nil, "/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", "Payload", output); err != nil {
		return "", &fastPackageFallbackError{err: err}
	}
	r.packagingMS += time.Since(packageStarted).Milliseconds()
	return output, nil
}

func wildcardSigningEligible(project string) (bool, string) {
	if project == "" {
		return false, "workspace builds require explicit signing profiles"
	}
	data, err := os.ReadFile(filepath.Join(project, "project.pbxproj"))
	if err != nil {
		return false, "could not inspect Xcode project signing settings"
	}
	contents := string(data)
	if strings.Count(contents, "product-type.application\"") != 1 {
		return false, "project must contain exactly one application target"
	}
	if strings.Contains(contents, "product-type.app-extension") {
		return false, "project contains an app extension"
	}
	if hasIOSEntitlements(contents) {
		return false, "project uses an entitlements file"
	}
	if strings.Contains(contents, "SystemCapabilities = {") {
		return false, "project enables restricted capabilities"
	}
	return true, ""
}

// hasIOSEntitlements reports whether the project applies an entitlements file to
// the iOS build. Settings scoped to another SDK (e.g. CODE_SIGN_ENTITLEMENTS[sdk=macosx*])
// do not affect the iOS target and must not block wildcard signing.
func hasIOSEntitlements(contents string) bool {
	for _, line := range strings.Split(contents, "\n") {
		if !strings.Contains(line, "CODE_SIGN_ENTITLEMENTS") {
			continue
		}
		if strings.Contains(line, "CODE_SIGN_ENTITLEMENTS[") && !strings.Contains(line, "[sdk=iphone") {
			continue
		}
		return true
	}
	return false
}

func findProvisioningProfile(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, dir := range []string{
		filepath.Join(home, "Library", "MobileDevice", "Provisioning Profiles"),
		filepath.Join(home, "Library", "Developer", "Xcode", "UserData", "Provisioning Profiles"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			switch strings.ToLower(filepath.Ext(entry.Name())) {
			case ".mobileprovision", ".provisionprofile":
				candidates = append(candidates, filepath.Join(dir, entry.Name()))
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, leftErr := os.Stat(candidates[i])
		right, rightErr := os.Stat(candidates[j])
		if leftErr != nil || rightErr != nil {
			return false
		}
		return left.ModTime().After(right.ModTime())
	})
	for _, path := range candidates {
		data, err := exec.Command("/usr/bin/security", "cms", "-D", "-i", path).Output()
		if err != nil {
			continue
		}
		if provisioningProfileName(data) == name {
			return path, nil
		}
	}
	return "", fmt.Errorf("provisioning profile %q is not installed under ~/Library/MobileDevice/Provisioning Profiles", name)
}

func provisioningProfileName(data []byte) string {
	const key = "<key>Name</key>"
	index := bytes.Index(data, []byte(key))
	if index < 0 {
		return ""
	}
	rest := data[index+len(key):]
	start := bytes.Index(rest, []byte("<string>"))
	if start < 0 {
		return ""
	}
	rest = rest[start+len("<string>"):]
	end := bytes.Index(rest, []byte("</string>"))
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(string(rest[:end]))
}

func embedProvisioningProfile(app, profilePath string) error {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("read provisioning profile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(app, "embedded.mobileprovision"), data, 0o644); err != nil {
		return fmt.Errorf("embed provisioning profile: %w", err)
	}
	return nil
}

func (r *jobRunner) buildExpo(ctx context.Context, source string, spec BuildSpec) (string, error) {
	root := filepath.Join(r.jobDir, "source")
	if err := unzip(source, root); err != nil {
		return "", fmt.Errorf("extract source: %w", err)
	}
	root = collapseSingleDirectory(root)
	return r.buildExpoAt(ctx, root, spec, filepath.Join(r.jobDir, "DerivedData"))
}

func (r *jobRunner) buildExpoAt(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, "package.json")); err != nil {
		return "", fmt.Errorf("package.json not found at project root")
	}

	var install []string
	var dependencyFiles []string
	dependenciesLocked := true
	switch {
	case fileExists(filepath.Join(root, "pnpm-lock.yaml")):
		install = []string{"pnpm", "install", "--frozen-lockfile"}
		dependencyFiles = []string{"package.json", "pnpm-lock.yaml"}
	case fileExists(filepath.Join(root, "yarn.lock")):
		install = []string{"yarn", "install", "--frozen-lockfile"}
		dependencyFiles = []string{"package.json", "yarn.lock"}
	case fileExists(filepath.Join(root, "bun.lock")) || fileExists(filepath.Join(root, "bun.lockb")):
		install = []string{"bun", "install", "--frozen-lockfile"}
		lock := "bun.lock"
		if !fileExists(filepath.Join(root, lock)) {
			lock = "bun.lockb"
		}
		dependencyFiles = []string{"package.json", lock}
	case fileExists(filepath.Join(root, "package-lock.json")):
		install = []string{"npm", "ci"}
		dependencyFiles = []string{"package.json", "package-lock.json"}
	default:
		install = []string{"npm", "install"}
		dependencyFiles = []string{"package.json"}
		dependenciesLocked = false
	}
	dependencyState := filepath.Join(filepath.Dir(derivedData), "dependency-state", "javascript.json")
	dependencyHash, fingerprintErr := dependencyFingerprint(root, dependencyFiles, toolVersion(install[0]))
	if fingerprintErr != nil {
		return "", fingerprintErr
	}
	if dependenciesLocked && dependencyCacheHit(dependencyState, dependencyHash, filepath.Join(root, "node_modules")) {
		r.dependencyHit = true
		r.agent.log(ctx, r.buildID, "JavaScript dependency cache hit: install skipped\n")
	} else {
		started := time.Now()
		if err := r.command(ctx, root, nil, install[0], install[1:]...); err != nil {
			return "", err
		}
		r.dependencyMS += time.Since(started).Milliseconds()
		if dependenciesLocked {
			if err := storeDependencyCacheState(dependencyState, dependencyHash); err != nil {
				r.agent.log(ctx, r.buildID, "warning: store JavaScript dependency cache state: "+err.Error()+"\n")
			}
		}
	}
	started := time.Now()
	if err := r.command(ctx, root, []string{"CI=1", "EXPO_NO_DOCTOR=1"}, "npx", "expo", "prebuild", "--platform", "ios", "--non-interactive"); err != nil {
		return "", err
	}
	r.prebuildMS += time.Since(started).Milliseconds()
	iosDir := filepath.Join(root, "ios")
	if fileExists(filepath.Join(iosDir, "Podfile")) {
		if _, err := exec.LookPath("pod"); err != nil {
			return "", fmt.Errorf("CocoaPods is required: install it with `brew install cocoapods`")
		}
		podEnv, envErr := r.agent.xcodeEnvironment()
		if envErr != nil {
			return "", envErr
		}
		cocoaPodsHome := filepath.Join(r.agent.cfg.CacheRoot, "dependencies", "cocoapods")
		if err := os.MkdirAll(cocoaPodsHome, 0o750); err != nil {
			return "", err
		}
		podEnv = append(podEnv, "CP_HOME_DIR="+cocoaPodsHome)
		podFiles := []string{"Podfile"}
		podsLocked := fileExists(filepath.Join(iosDir, "Podfile.lock"))
		if podsLocked {
			podFiles = append(podFiles, "Podfile.lock")
		}
		podHash, fingerprintErr := dependencyFingerprint(iosDir, podFiles, toolVersion("pod"))
		if fingerprintErr != nil {
			return "", fingerprintErr
		}
		podState := filepath.Join(filepath.Dir(derivedData), "dependency-state", "cocoapods.json")
		manifestLock := filepath.Join(iosDir, "Pods", "Manifest.lock")
		if podsLocked && dependencyCacheHit(podState, podHash, manifestLock) &&
			filesEqual(filepath.Join(iosDir, "Podfile.lock"), manifestLock) {
			r.podCacheHit = true
			r.agent.log(ctx, r.buildID, "CocoaPods dependency cache hit: pod install skipped\n")
		} else {
			started = time.Now()
			if err := r.command(ctx, iosDir, podEnv, "pod", "install"); err != nil {
				return "", err
			}
			r.dependencyMS += time.Since(started).Milliseconds()
			if podsLocked {
				if err := storeDependencyCacheState(podState, podHash); err != nil {
					r.agent.log(ctx, r.buildID, "warning: store CocoaPods dependency cache state: "+err.Error()+"\n")
				}
			}
		}
	}
	return r.buildXcodeAt(ctx, iosDir, spec, derivedData)
}

func (r *jobRunner) archiveAndExport(ctx context.Context, root string, spec BuildSpec, derivedData string) (string, error) {
	r.strategy = "archive-export"
	xcodeEnv, err := r.agent.signingEnvironment()
	if err != nil {
		return "", err
	}
	workspace, project, err := findXcodeContainer(root, spec)
	if err != nil {
		return "", err
	}
	containerArgs := []string{}
	if workspace != "" {
		containerArgs = []string{"-workspace", workspace}
	} else {
		containerArgs = []string{"-project", project}
	}
	scheme := spec.Scheme
	if scheme == "" {
		scheme, err = r.discoverScheme(ctx, root, containerArgs)
		if err != nil {
			return "", err
		}
	}
	configuration := spec.Configuration
	if configuration == "" {
		configuration = "Release"
	}
	teamID := spec.TeamID
	if teamID == "" {
		teamID = r.agent.cfg.TeamID
	}
	if teamID == "" {
		return "", fmt.Errorf("team ID is required in the build spec or APPLE_TEAM_ID")
	}
	archivePath := filepath.Join(r.jobDir, "build", "App.xcarchive")
	exportDir := filepath.Join(r.jobDir, "export")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o750); err != nil {
		return "", err
	}
	args := append([]string{}, containerArgs...)
	args = append(args, "-scheme", scheme, "-configuration", configuration)
	args = append(args, r.packageResolutionArgs(root)...)
	args = append(args, xcodeTrustArgs()...)
	args = append(args, "-parallelizeTargets", "-destination", "generic/platform=iOS",
		"-archivePath", archivePath, "-derivedDataPath", derivedData,
		"DEVELOPMENT_TEAM="+teamID, "archive")
	if spec.AllowProvisioningUpdates {
		args = append(args, r.provisioningAuthArgs()...)
	}
	started := time.Now()
	if err := r.xcodebuildCompile(ctx, root, r.withXcodeVersion(xcodeEnv), args); err != nil {
		return "", err
	}
	r.archiveMS += time.Since(started).Milliseconds()
	method := spec.ExportMethod
	if method == "" {
		method = r.agent.cfg.ExportMethod
	}
	method = currentExportMethod(method)
	exportPlist := filepath.Join(r.jobDir, "ExportOptions.plist")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>method</key><string>%s</string><key>teamID</key><string>%s</string><key>signingStyle</key><string>automatic</string><key>stripSwiftSymbols</key><true/></dict></plist>`, xmlEscape(method), xmlEscape(teamID))
	if err := os.WriteFile(exportPlist, []byte(plist), 0o600); err != nil {
		return "", err
	}
	exportArgs := []string{"-exportArchive", "-archivePath", archivePath, "-exportPath", exportDir, "-exportOptionsPlist", exportPlist}
	if spec.AllowProvisioningUpdates {
		exportArgs = append(exportArgs, r.provisioningAuthArgs()...)
	}
	started = time.Now()
	if err := r.command(ctx, root, r.withXcodeVersion(xcodeEnv), "xcodebuild", exportArgs...); err != nil {
		return "", err
	}
	r.exportMS += time.Since(started).Milliseconds()
	return findFirstFile(exportDir, ".ipa")
}

func (r *jobRunner) packageResolutionArgs(root string) []string {
	var args []string
	if hasPackageResolved(root) {
		args = append(args, "-skipPackageUpdates", "-onlyUsePackageVersionsFromResolvedFile")
	}
	return args
}

// Non-interactive workers cannot answer Xcode's macro/plugin trust prompts, so
// packages that are valid but not yet trusted on this worker would otherwise
// fail the dependency graph with "must be enabled before it can be used".
func xcodeTrustArgs() []string {
	return []string{"-skipPackagePluginValidation", "-skipMacroValidation"}
}

func hasPackageResolved(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && path != root {
			switch entry.Name() {
			case ".git", "DerivedData", "build", "node_modules":
				return filepath.SkipDir
			}
		}
		if !entry.IsDir() && entry.Name() == "Package.resolved" {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func (r *jobRunner) provisioningAuthArgs() []string {
	args := []string{"-allowProvisioningUpdates"}
	if r.agent.cfg.APIKeyPath != "" && r.agent.cfg.APIKeyID != "" && r.agent.cfg.APIIssuerID != "" {
		args = append(args, "-authenticationKeyPath", r.agent.cfg.APIKeyPath,
			"-authenticationKeyID", r.agent.cfg.APIKeyID,
			"-authenticationKeyIssuerID", r.agent.cfg.APIIssuerID)
	}
	return args
}

func (r *jobRunner) discoverScheme(ctx context.Context, root string, containerArgs []string) (string, error) {
	if scheme, ok := uniqueSharedScheme(root, containerArgs); ok {
		r.agent.log(ctx, r.buildID, "Using unique shared Xcode scheme: "+scheme+"\n")
		return scheme, nil
	}
	xcodeEnv, err := r.agent.xcodeEnvironment()
	if err != nil {
		return "", err
	}
	args := append(append([]string{}, containerArgs...), "-list", "-json")
	output, err := r.commandOutput(ctx, root, r.withXcodeVersion(xcodeEnv), "xcodebuild", args...)
	if err != nil {
		return "", err
	}
	var listing struct {
		Workspace struct {
			Schemes []string `json:"schemes"`
		} `json:"workspace"`
		Project struct {
			Schemes []string `json:"schemes"`
		} `json:"project"`
	}
	if err := json.Unmarshal(output, &listing); err != nil {
		return "", fmt.Errorf("parse xcodebuild scheme list: %w", err)
	}
	schemes := listing.Workspace.Schemes
	if len(schemes) == 0 {
		schemes = listing.Project.Schemes
	}
	if len(schemes) == 0 {
		return "", fmt.Errorf("no shared Xcode scheme found; set spec.scheme explicitly")
	}
	return schemes[0], nil
}

func uniqueSharedScheme(root string, containerArgs []string) (string, bool) {
	var project string
	for index := 0; index+1 < len(containerArgs); index++ {
		if containerArgs[index] == "-project" {
			project = containerArgs[index+1]
			break
		}
	}

	var paths []string
	if project != "" {
		paths, _ = filepath.Glob(filepath.Join(project, "xcshareddata", "xcschemes", "*.xcscheme"))
	} else {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() && path != root {
				switch entry.Name() {
				case ".git", ".build", "DerivedData", "node_modules", "Pods":
					return filepath.SkipDir
				}
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".xcscheme" {
				return nil
			}
			parent := filepath.Dir(path)
			if filepath.Base(parent) == "xcschemes" && filepath.Base(filepath.Dir(parent)) == "xcshareddata" {
				paths = append(paths, path)
			}
			return nil
		})
	}

	schemes := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		schemes[strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))] = struct{}{}
	}
	if len(schemes) != 1 {
		return "", false
	}
	for scheme := range schemes {
		return scheme, true
	}
	return "", false
}

func (r *jobRunner) resignIPA(ctx context.Context, source string) (string, error) {
	if r.agent.cfg.ProvisioningProfile == "" {
		return "", fmt.Errorf("IOS_PROVISIONING_PROFILE is required for IPA resigning")
	}
	if !fileExists(r.agent.cfg.ProvisioningProfile) {
		return "", fmt.Errorf("provisioning profile does not exist: %s", r.agent.cfg.ProvisioningProfile)
	}
	root := filepath.Join(r.jobDir, "resign")
	if err := unzip(source, root); err != nil {
		return "", fmt.Errorf("extract IPA: %w", err)
	}
	apps, err := filepath.Glob(filepath.Join(root, "Payload", "*.app"))
	if err != nil || len(apps) != 1 {
		return "", fmt.Errorf("IPA must contain exactly one Payload/*.app")
	}
	app := apps[0]
	profileTarget := filepath.Join(app, "embedded.mobileprovision")
	if err := copyFile(r.agent.cfg.ProvisioningProfile, profileTarget); err != nil {
		return "", err
	}
	decoded := filepath.Join(r.jobDir, "profile.plist")
	entitlements := filepath.Join(r.jobDir, "entitlements.plist")
	if err := r.command(ctx, r.jobDir, nil, "security", "cms", "-D", "-i", r.agent.cfg.ProvisioningProfile, "-o", decoded); err != nil {
		return "", err
	}
	if err := r.command(ctx, r.jobDir, nil, "plutil", "-extract", "Entitlements", "xml1", "-o", entitlements, decoded); err != nil {
		return "", err
	}
	_ = os.RemoveAll(filepath.Join(app, "_CodeSignature"))
	nested, err := nestedCodeObjects(app)
	if err != nil {
		return "", err
	}
	for _, path := range nested {
		if err := r.command(ctx, r.jobDir, nil, "codesign", "--force", "--sign", r.agent.cfg.SigningIdentity, "--timestamp=none", path); err != nil {
			return "", err
		}
	}
	if err := r.command(ctx, r.jobDir, nil, "codesign", "--force", "--sign", r.agent.cfg.SigningIdentity,
		"--entitlements", entitlements, "--timestamp=none", app); err != nil {
		return "", err
	}
	output := filepath.Join(r.jobDir, "resigned.ipa")
	if err := r.command(ctx, root, nil, "/usr/bin/ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", "Payload", output); err != nil {
		return "", err
	}
	return output, nil
}

func nestedCodeObjects(app string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(app, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == app {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if entry.IsDir() && (ext == ".framework" || ext == ".appex" || ext == ".app" || ext == ".xpc") {
			paths = append(paths, path)
			return filepath.SkipDir
		}
		if !entry.IsDir() && ext == ".dylib" {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Slice(paths, func(i, j int) bool {
		return strings.Count(paths[i], string(os.PathSeparator)) > strings.Count(paths[j], string(os.PathSeparator))
	})
	return paths, err
}

func findSimulatorApp(root string) (string, error) {
	var apps []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".app") && strings.Contains(path, "iphonesimulator") {
			apps = append(apps, path)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(apps)
	if len(apps) == 0 {
		return "", fmt.Errorf("no simulator .app found under %s", root)
	}
	for _, app := range apps {
		if !strings.Contains(strings.ToLower(filepath.Base(app)), "test") {
			return app, nil
		}
	}
	return apps[0], nil
}

func findDeviceApp(root string) (string, error) {
	var apps []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".app") && strings.Contains(path, "-iphoneos") {
			apps = append(apps, path)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(apps)
	if len(apps) == 0 {
		return "", fmt.Errorf("no device .app found under %s", root)
	}
	for _, app := range apps {
		name := strings.ToLower(filepath.Base(app))
		if !strings.Contains(name, "test") && !strings.Contains(app, ".appex/") {
			return app, nil
		}
	}
	return apps[0], nil
}

func (r *jobRunner) command(ctx context.Context, dir string, env []string, name string, args ...string) error {
	_, err := r.commandOutput(ctx, dir, env, name, args...)
	return err
}

func (r *jobRunner) commandOutput(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	r.agent.log(ctx, r.buildID, "$ "+r.agent.redact(shellLine(name, args))+"\n")
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = childEnvironment(env)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if cmd.ProcessState != nil {
		r.cpuUserMS += cmd.ProcessState.UserTime().Milliseconds()
		r.cpuSystemMS += cmd.ProcessState.SystemTime().Milliseconds()
		if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			peak := usage.Maxrss
			if runtime.GOOS != "darwin" {
				peak *= 1024
			}
			if peak > r.peakRSSBytes {
				r.peakRSSBytes = peak
			}
		}
	}
	if output.Len() > 0 {
		r.agent.log(ctx, r.buildID, output.String())
	}
	if err != nil {
		return output.Bytes(), fmt.Errorf("%s failed: %w", name, err)
	}
	return output.Bytes(), nil
}

func childEnvironment(overrides []string) []string {
	overridden := make(map[string]struct{}, len(overrides))
	for _, value := range overrides {
		if index := strings.IndexByte(value, '='); index > 0 {
			overridden[value[:index]] = struct{}{}
		}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides)+1)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "XPC_SERVICE_NAME=") || strings.HasPrefix(value, "XPC_FLAGS=") {
			continue
		}
		if index := strings.IndexByte(value, '='); index > 0 {
			if _, exists := overridden[value[:index]]; exists {
				continue
			}
		}
		environment = append(environment, value)
	}
	environment = append(environment, "__CF_USER_TEXT_ENCODING=0x1F5:0x19:0x34")
	return append(environment, overrides...)
}

func shellLine(name string, args []string) string {
	parts := []string{name}
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\n\"'") {
			parts = append(parts, fmt.Sprintf("%q", arg))
		} else {
			parts = append(parts, arg)
		}
	}
	return strings.Join(parts, " ")
}

func unzip(source, destination string) error {
	reader, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	cleanRoot := filepath.Clean(destination) + string(os.PathSeparator)
	for _, file := range reader.File {
		target := filepath.Join(destination, file.Name)
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanRoot) {
			return fmt.Errorf("archive contains unsafe path %q", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if file.Mode()&os.ModeSymlink != 0 {
			rc, err := file.Open()
			if err != nil {
				return err
			}
			linkData, readErr := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if readErr != nil {
				return readErr
			}
			link := string(linkData)
			if !safeManifestPath(link) {
				return fmt.Errorf("archive contains unsafe symlink %q -> %q", file.Name, link)
			}
			if err := materializeSymlink(target, link); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		rc, err := file.Open()
		if err != nil {
			return err
		}
		mode := file.Mode()
		if mode == 0 {
			mode = 0o640
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func collapseSingleDirectory(root string) string {
	entries, err := os.ReadDir(root)
	if err == nil && len(entries) == 1 && entries[0].IsDir() && entries[0].Name() != "Payload" {
		return filepath.Join(root, entries[0].Name())
	}
	return root
}

func findXcodeContainer(root string, spec BuildSpec) (string, string, error) {
	if spec.Workspace != "" {
		return filepath.Join(root, spec.Workspace), "", nil
	}
	if spec.Project != "" {
		return "", filepath.Join(root, spec.Project), nil
	}
	var workspaces, projects []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == ".git") {
			return filepath.SkipDir
		}
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".xcworkspace") && !strings.Contains(path, ".xcodeproj/") {
			workspaces = append(workspaces, path)
			return filepath.SkipDir
		}
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".xcodeproj") {
			projects = append(projects, path)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(workspaces)
	sort.Strings(projects)
	if len(workspaces) > 0 {
		return workspaces[0], "", nil
	}
	if len(projects) > 0 {
		return "", projects[0], nil
	}
	return "", "", fmt.Errorf("no .xcworkspace or .xcodeproj found")
}

func findFirstFile(root, extension string) (string, error) {
	var found string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.EqualFold(filepath.Ext(path), extension) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("no %s artifact found under %s", extension, root)
	}
	return found, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func currentExportMethod(method string) string {
	switch method {
	case "development":
		return "debugging"
	case "ad-hoc":
		return "release-testing"
	case "app-store":
		return "app-store-connect"
	default:
		return method
	}
}
